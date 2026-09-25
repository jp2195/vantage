// Loadgen: many simulated routers speaking real BMP at a real collector, to
// find where the pipeline saturates.
//
// This is not bmpgen with bigger numbers, and the difference is why it is a
// separate command. bmpgen builds every message in memory before writing a
// byte, speaks from one connection as one router, and derives its prefixes as
// 10.{peer}.{update}.0/24 -- both indices bytes, so it tops out at 65,536
// distinct prefixes and then silently repeats them. Repeats collapse under
// ReplacingMergeTree, so a scale test built on it would understate its own row
// count while reporting success. bmpgen is a correctness fixture: a handful of
// messages, shaped precisely, with a real vendor banner. This is a firehose.
//
// It exists in the repo rather than in a scratchpad because it has now been
// written twice. An earlier one-off generator for this job collapsed
// distinct prefixes onto few output values, and a rewrite reproduced the
// same class of bug (see loadgen_test.go, which pins the fix).
//
// Router identity is the TCP source address, so each simulated router dials
// from its own 127.x.y.z -- the only way to present a fleet from one host.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
)

// loadgenBlockSize is how many distinct prefixes one prefix length yields: the
// first octet ranges over 11..110 (100 values, clear of 0/8, 127/8 and 224+)
// and the second and third octets range fully.
const loadgenBlockSize = 100 * 256 * 256

// loadgenPrefixAt maps a global index to a distinct, canonical prefix.
//
// The index goes into octets 1-3 and the fourth is always zero, because a /24
// MASKS OFF the fourth octet. Writing the index there is not a hypothetical
// mistake: done that way, 4,000 indexes produced 16 distinct prefixes.
//
// Prefix length is the second dimension rather than a wider address range,
// because 11.0.0.0/24 and 11.0.0.0/25 are distinct prefixes and both are
// ordinary things to see in a real table. Four blocks gives 26.2M distinct
// prefixes, comfortably past a dual-homed full table.
func loadgenPrefixAt(i int) netip.Prefix {
	block, j := i/loadgenBlockSize, i%loadgenBlockSize
	if block > 3 {
		log.Fatalf("prefix index %d exceeds the %d-prefix space this mapping covers",
			i, 4*loadgenBlockSize)
	}
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{
		byte(11 + j/(256*256)), byte(j >> 8), byte(j), 0,
	}), 24+block)
}

func loadgenCaps() bgp.Caps {
	return bgp.Caps{
		FourByteAS:  true,
		MP:          map[bgp.Family]bool{bgp.FamilyIPv4U: true},
		AddPathRecv: map[bgp.Family]bool{},
	}
}

type loadgenCounters struct{ msgs, prefixes, bytes atomic.Int64 }

func cmdLoadgen(args []string) error {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	target := fs.String("target", "127.0.0.1:11019", "collector address")
	routers := fs.Int("routers", 20, "simulated routers (one TCP session each)")
	peers := fs.Int("peers", 8, "simulated peers per router")
	prefixes := fs.Int("prefixes", 20000, "prefixes per peer")
	perUpdate := fs.Int("nlri-per-update", 1,
		"prefixes packed into one BGP UPDATE; this is the variable that decides "+
			"both throughput and bytes-per-row, so a run that does not state it "+
			"has not stated its result")
	aspathLen := fs.Int("aspath", 4, "AS path length (real internet paths are 4-8)")
	warmup := fs.Duration("warmup", 2*time.Second, "settle time after peer-up before the dump")
	hold := fs.Duration("hold", 0,
		"keep every BMP session open this long after the dump; without it the "+
			"sessions close and the collector correctly marks every peer "+
			"view_lost, so none of the loaded routes are current state any more")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *perUpdate < 1 {
		return fmt.Errorf("-nlri-per-update must be at least 1, got %d", *perUpdate)
	}

	total := *routers * *peers * *prefixes
	fmt.Printf("plan: %d routers x %d peers x %d prefixes = %d prefixes, "+
		"%d per UPDATE -> %d route-monitoring messages\n",
		*routers, *peers, *prefixes, total, *perUpdate, total / *perUpdate)

	var c loadgenCounters
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, *routers)

	for r := 0; r < *routers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			if err := loadgenRouter(r, *target, *peers, *prefixes, *perUpdate,
				*aspathLen, *hold, &c, start); err != nil {
				errs <- fmt.Errorf("router %d: %w", r, err)
			}
		}(r)
	}

	// The dump is what is being timed, so every session is brought up first
	// and the clock starts only once they all are. Timing the peer-up phase
	// too would measure connection setup and report it as ingest.
	time.Sleep(*warmup)
	t0 := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(t0)

	close(errs)
	var failed int
	for err := range errs {
		failed++
		if failed <= 5 {
			fmt.Fprintln(fs.Output(), err)
		}
	}

	msgs, pfx, byts := c.msgs.Load(), c.prefixes.Load(), c.bytes.Load()
	fmt.Printf("\nsent   %d messages, %d prefixes, %.2f MiB in %s\n",
		msgs, pfx, float64(byts)/(1<<20), elapsed.Round(time.Millisecond))
	fmt.Printf("rate   %.0f msg/s, %.0f prefixes/s\n",
		float64(msgs)/elapsed.Seconds(), float64(pfx)/elapsed.Seconds())
	// The send rate is what reached the socket, NOT what the pipeline
	// absorbed: the collector's receive buffer accepts a burst that has not
	// been decoded, published or written yet. End-to-end throughput is
	// measured by watching rows land in ClickHouse, not here.
	fmt.Println("note   this is the SEND rate; end-to-end throughput is what " +
		"lands in ClickHouse, and is lower")
	if failed > 0 {
		return fmt.Errorf("%d of %d routers failed", failed, *routers)
	}
	return nil
}

func loadgenRouter(r int, target string, peers, prefixes, perUpdate, aspathLen int,
	hold time.Duration, c *loadgenCounters, start <-chan struct{}) error {
	// A distinct loopback source per router, never 127.0.0.1 itself, which is
	// what the generator's own host uses.
	src := netip.AddrFrom4([4]byte{127, byte(r >> 16), byte((r >> 8) + 1), byte(r%254 + 1)})
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: src.AsSlice()}}
	conn, err := d.Dial("tcp", target)
	if err != nil {
		return fmt.Errorf("dial from %s: %w", src, err)
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4 << 20)
	}

	write := func(b []byte) error {
		if _, err := conn.Write(b); err != nil {
			return err
		}
		c.msgs.Add(1)
		c.bytes.Add(int64(len(b)))
		return nil
	}

	if err := write(bmptest.Init(fmt.Sprintf("loadgen-r%03d", r),
		"Cisco IOS XR Software, Version 7.9.2")); err != nil {
		return err
	}

	hdrs := make([]bmp.PeerHeader, peers)
	for p := range hdrs {
		peerIP := netip.AddrFrom4([4]byte{10, byte(r >> 8), byte(r), byte(p + 1)})
		hdrs[p] = bmp.PeerHeader{
			Type: bmp.PeerTypeGlobal, Addr: peerIP,
			AS: uint32(65000 + p), BGPID: peerIP.String(), Timestamp: time.Now(),
		}
		if err := write(bmptest.PeerUp(hdrs[p], src, 179, uint16(33000+p),
			loadgenCaps(), loadgenCaps(), 65000, uint32(65000+p))); err != nil {
			return err
		}
	}

	<-start
	if hold > 0 {
		defer time.Sleep(hold)
	}

	// Each router owns a contiguous, non-overlapping slice of the global
	// prefix space, so distinctness is a property of the partition rather than
	// of anything happening at run time.
	base := r * peers * prefixes
	asPath := make([]uint32, aspathLen)
	for i := range asPath {
		asPath[i] = uint32(64500 + i)
	}

	batch := make([]bgp.Prefix, 0, perUpdate)
	flush := func(p int) error {
		if len(batch) == 0 {
			return nil
		}
		if err := write(bmptest.RouteMonitoring(hdrs[p], bgp.BuildUpdate{
			Announced: batch, ASPath: asPath, FourByteAS: true, NextHop: hdrs[p].Addr,
		})); err != nil {
			return err
		}
		c.prefixes.Add(int64(len(batch)))
		batch = batch[:0]
		return nil
	}

	for p := range peers {
		asPath[len(asPath)-1] = hdrs[p].AS
		for n := range prefixes {
			batch = append(batch, bgp.Prefix{Prefix: loadgenPrefixAt(base + p*prefixes + n)})
			if len(batch) < perUpdate {
				continue
			}
			if err := flush(p); err != nil {
				return err
			}
		}
		if err := flush(p); err != nil {
			return err
		}
	}
	return nil
}
