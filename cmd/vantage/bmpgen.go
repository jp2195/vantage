// bmpgen.go implements `vantage bmpgen`: a synthetic BMP router that dials a
// collector and speaks a realistic session -- Initiation once, then per
// simulated peer a Peer-Up (so the collector has negotiated capabilities on
// record) before any Route Monitoring, `updates` UPDATEs, and one Stats
// Report -- mirroring the sequence a real router sends rather than
// exercising the collector's "capabilities unknown" fallback path.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"time"

	"strings"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
)

// genMessages builds the wire-ready BMP message sequence bmpgen sends: one
// Initiation, then per simulated peer (IPv4 10.0.0.9+i, ASN 65001+i) a
// Peer-Up (advertising ipv4u MP + 4-byte-AS capabilities in both
// directions), `updates` Route Monitoring UPDATEs (prefixes
// 10.{i}.{n}.0/24, next-hop the peer's own address, AS path
// [peer-ASN, 64512]), and one Stats Report. Split out from cmdBmpgen so it
// is unit-testable directly (parse each message back through
// bmp.ReadMsg) without a network connection.
func genMessages(router, sysDescr string, peers, updates int) [][]byte {
	msgs := [][]byte{bmptest.Init(router, sysDescr)}
	caps := bgp.Caps{
		FourByteAS:  true,
		MP:          map[bgp.Family]bool{bgp.FamilyIPv4U: true},
		AddPathRecv: map[bgp.Family]bool{},
	}
	for i := range peers {
		peerIP := netip.AddrFrom4([4]byte{10, 0, 0, byte(9 + i)})
		ph := bmp.PeerHeader{
			Type: bmp.PeerTypeGlobal, Addr: peerIP, AS: uint32(65001 + i),
			BGPID: peerIP.String(), Timestamp: time.Now(),
		}
		msgs = append(msgs, bmptest.PeerUp(ph, netip.AddrFrom4([4]byte{10, 0, 0, 1}),
			179, 33001, caps, caps, 65000, ph.AS))
		for n := range updates {
			msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
				Announced: []bgp.Prefix{{Prefix: netip.PrefixFrom(
					netip.AddrFrom4([4]byte{10, byte(i), byte(n), 0}), 24)}},
				Origin: 0, ASPath: []uint32{ph.AS, 64512}, FourByteAS: true,
				NextHop: peerIP,
			}))
		}
		msgs = append(msgs, bmptest.Stats(ph, map[uint32]uint64{0: uint64(updates), 7: 0}))
	}
	return msgs
}

// profileByName resolves a -profile token to one of the vendor identities
// measured off committed captures. Unknown names are an error rather than a
// fallback: the whole point of the profile table is that an identity bmpgen
// emits has been observed from that implementation, and silently substituting
// a default for an unrecognized vendor would put invented-looking traffic on
// the wire under a name the operator asked for.
func profileByName(name string) (bmptest.Profile, error) {
	profiles := bmptest.Profiles()
	for _, p := range profiles {
		if p.Name == name {
			return p, nil
		}
	}
	known := make([]string, 0, len(profiles))
	for _, p := range profiles {
		known = append(known, p.Name)
	}
	return bmptest.Profile{}, fmt.Errorf(
		"no profile %q: vantage has no committed BMP capture from that implementation, "+
			"so its Initiation banner is unknown; measured profiles are %s",
		name, strings.Join(known, ", "))
}

// genChurn builds `rounds` announce/withdraw cycles over `prefixes` prefixes
// for one peer, which is the part of a synthetic feed a corpus of captures
// cannot substitute for: a committed fixture is a handful of messages frozen
// at one instant, while CI wants a feed that keeps moving.
//
// Each round announces the whole set and then withdraws half of it, so both
// the insert and the delete path are exercised every cycle. Announce-only
// traffic is the easy mistake here and it never reaches the sink's dedupe or
// the "did this prefix go away" logic the dashboards are built on.
//
// Prefixes are 10.{round}.{n}.0/24, deterministic in the round and index, so a
// run is reproducible and a failure names a prefix that can be found again.
func genChurn(ph bmp.PeerHeader, prefixes, rounds int) [][]byte {
	var msgs [][]byte
	for r := range rounds {
		var all []bgp.Prefix
		for n := range prefixes {
			all = append(all, bgp.Prefix{Prefix: netip.PrefixFrom(
				netip.AddrFrom4([4]byte{10, byte(r), byte(n), 0}), 24)})
		}
		msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
			Announced: all, Origin: 0, ASPath: []uint32{ph.AS, 64512},
			FourByteAS: true, NextHop: ph.Addr,
		}))
		// Withdraw half. Withdrawing everything just announced would leave the
		// steady-state table empty, which is not what a real router's churn
		// looks like and gives the sink nothing to hold between rounds.
		half := all[:len(all)/2+1]
		msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
			Withdrawn: half, Origin: 0, ASPath: []uint32{ph.AS, 64512},
			FourByteAS: true, NextHop: ph.Addr,
		}))
	}
	return msgs
}

// sendMessages writes msgs to w in order. The first dumpLen -- the
// Initiation, Peer Ups and initial dump -- go back to back; each message
// after them waits interval first. Unpaced, a churn run of thousands of
// rounds is over in seconds, and a feed meant to keep moving while
// something else happens (a NATS rolling restart, say) has already gone
// quiet by the time it matters.
func sendMessages(w io.Writer, msgs [][]byte, dumpLen int, interval time.Duration, wait func(time.Duration)) error {
	for i, m := range msgs {
		if i >= dumpLen && interval > 0 {
			wait(interval)
		}
		if _, err := w.Write(m); err != nil {
			return err
		}
	}
	return nil
}

// defaultSysName is the device name bmpgen uses when -router is not given.
// See the note at its call site for why it is not the profile's own SysName.
func defaultSysName(p bmptest.Profile) string { return "bmpgen-" + p.Name }

// genChurnFamily is genChurn for a non-IPv4-unicast family. Same shape --
// announce a set, withdraw half of it, repeat -- so the two produce comparable
// load; what differs is the encoding, which AppendUpdate handles from the
// family alone.
//
// Prefixes are derived from the round and index so a run is reproducible and a
// failure names something findable. VPN entries get an RD per peer and a label
// per prefix, because a VPN table keyed on prefix alone is not what the
// product stores: route_vpn is keyed by (RD, prefix), and a generator that
// emitted one RD for everything would never exercise that.
// parseFamilies resolves the -families tokens. The names are the same ones the
// sink writes into its family column (see sink/rows.go's familyNames), so what
// an operator asks the generator for reads identically to what turns up in
// ClickHouse afterwards.
func parseFamilies(s string) ([]bgp.Family, error) {
	known := map[string]bgp.Family{
		"ipv4u": bgp.FamilyIPv4U,
		"ipv6u": bgp.FamilyIPv6U,
		"vpn4":  bgp.FamilyVPNv4,
		"vpn6":  bgp.FamilyVPNv6,
		"lu4":   bgp.FamilyLU4,
		"evpn":  bgp.FamilyEVPN,
		"ls":    bgp.FamilyBGPLS,
	}
	var out []bgp.Family
	for tok := range strings.SplitSeq(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		f, ok := known[tok]
		if !ok {
			return nil, fmt.Errorf("unknown family %q; known: ipv4u, ipv6u, vpn4, vpn6, lu4, evpn, ls", tok)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-families is empty")
	}
	return out, nil
}

func genChurnFamily(ph bmp.PeerHeader, fam bgp.Family, prefixes, rounds int) [][]byte {
	var msgs [][]byte
	v6 := fam == bgp.FamilyVPNv6 || fam == bgp.FamilyIPv6U
	nextHop := ph.Addr
	if v6 {
		nextHop = netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	}

	for r := range rounds {
		var labeled []bgp.VpnPrefix
		var plain []bgp.Prefix
		var evpn []bgp.EvpnRoute
		if fam == bgp.FamilyBGPLS {
			// Node NLRI only -- Link and Prefix NLRI have no builder yet, so
			// asking for them would emit nothing rather than something wrong.
			var nodes []bgp.LsNodeNLRI
			for n := range prefixes {
				nodes = append(nodes, bgp.LsNodeNLRI{
					Protocol: 2, Identifier: uint64(r),
					Local: bgp.LsNodeDescriptor{
						ASN: ph.AS, BGPLSID: 0, Area: uint32(r),
						// A 6-byte IS-IS system ID, because Protocol 2 says
						// IS-IS Level 2. RFC 9552 5.2.1.4 makes the IGP
						// Router-ID width protocol-specific -- IS-IS 6 bytes
						// (7 with the pseudonode octet), OSPF 4 (8 for a LAN
						// pseudonode) -- and this used to emit the 4-byte OSPF
						// shape under an IS-IS protocol-ID, which no real
						// router sends.
						//
						// The generator writes into the same archive real
						// captures do, so an impossible shape here is not
						// harmless: width is the only way to tell a pseudonode
						// from a router, since neither IGP flags it, and 248
						// such rows are already sitting in the archive
						// looking like a decoder defect.
						RouterID: []byte{0x01, 0x02, 0x55, 0x00, byte(r), byte(n)},
					},
				})
			}
			msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
				Origin: 0, ASPath: []uint32{ph.AS}, FourByteAS: true,
				MP: &bgp.BuildMP{Family: fam, NextHop: ph.Addr, LsNodesAnnounced: nodes},
			}))
			msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
				MP: &bgp.BuildMP{Family: fam, NextHop: ph.Addr,
					LsNodesWithdrawn: nodes[:len(nodes)/2+1]},
			}))
			continue
		}
		if fam == bgp.FamilyEVPN {
			for n := range prefixes {
				// Type 2 MAC/IP, the route a host generates on a VXLAN
				// fabric and the one route_evpn is mostly made of. The
				// label is the VNI, written raw -- see AppendEvpnNLRI.
				evpn = append(evpn, bgp.EvpnRoute{
					RouteType: 2, RD: fmt.Sprintf("%d:%d", ph.AS, 32777+r),
					EthernetTag: 0,
					MAC:         fmt.Sprintf("52:54:00:%02x:%02x:%02x", r, n>>8, n&0xff),
					IP:          netip.AddrFrom4([4]byte{10, 200, byte(r), byte(n)}).String(),
					Labels:      []uint32{uint32(10010 + r)},
				})
			}
			msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
				Origin: 0, ASPath: []uint32{ph.AS, 64512}, FourByteAS: true,
				MP: &bgp.BuildMP{Family: fam, NextHop: ph.Addr, EvpnAnnounced: evpn},
			}))
			msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
				MP: &bgp.BuildMP{Family: fam, NextHop: ph.Addr,
					EvpnWithdrawn: evpn[:len(evpn)/2+1]},
			}))
			continue
		}
		for n := range prefixes {
			if v6 {
				a := [16]byte{0x20, 0x01, 0x0d, 0xb8, byte(r), byte(n)}
				pfx := netip.PrefixFrom(netip.AddrFrom16(a), 48)
				if fam == bgp.FamilyIPv6U {
					plain = append(plain, bgp.Prefix{Prefix: pfx})
				} else {
					labeled = append(labeled, bgp.VpnPrefix{Prefix: pfx,
						RD: fmt.Sprintf("%d:%d", ph.AS, r+1), Labels: []uint32{uint32(24000 + n)}})
				}
				continue
			}
			pfx := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(r), byte(n), 0}), 24)
			labeled = append(labeled, bgp.VpnPrefix{Prefix: pfx,
				RD: fmt.Sprintf("%d:%d", ph.AS, r+1), Labels: []uint32{uint32(24000 + n)}})
		}

		msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
			Origin: 0, ASPath: []uint32{ph.AS, 64512}, FourByteAS: true,
			MP: &bgp.BuildMP{Family: fam, NextHop: nextHop,
				Announced: labeled, PlainAnnounced: plain},
		}))

		half := func(n int) int { return n/2 + 1 }
		wl, wp := labeled, plain
		if len(wl) > 0 {
			wl = wl[:half(len(wl))]
		}
		if len(wp) > 0 {
			wp = wp[:half(len(wp))]
		}
		msgs = append(msgs, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
			MP: &bgp.BuildMP{Family: fam, NextHop: nextHop,
				Withdrawn: wl, PlainWithdrawn: wp},
		}))
	}
	return msgs
}

func cmdBmpgen(args []string) error {
	fs := flag.NewFlagSet("bmpgen", flag.ExitOnError)
	target := fs.String("target", "127.0.0.1:11019", "collector address")
	router := fs.String("router", "", "sysName (default: the profile's)")
	vendor := fs.String("vendor", "", "sysDescr (default: the profile's; overriding this emits an identity no router has been observed sending)")
	profile := fs.String("profile", "iosxr", "vendor identity to impersonate: iosxr, nxos, frr")
	peers := fs.Int("peers", 2, "simulated peers")
	updates := fs.Int("updates", 10, "updates per peer in the initial dump")
	churnPrefixes := fs.Int("churn-prefixes", 0, "prefixes per churn round (0 disables churn)")
	churnRounds := fs.Int("churn-rounds", 0, "announce/withdraw rounds per peer")
	families := fs.String("families", "ipv4u", "comma-separated families to churn: ipv4u,ipv6u,vpn4,vpn6,lu4")
	churnInterval := fs.Duration("churn-interval", 0, "wait between churn messages (0 sends them back to back)")
	fs.Parse(args)

	p, err := profileByName(*profile)
	if err != nil {
		return err
	}
	// Explicit flags win, so a caller can still craft an arbitrary banner --
	// but the default is a measured one, which is the opposite of how this
	// command shipped (its default sysDescr was a string no IOS-XR router
	// sends).
	if *router == "" {
		// Deliberately NOT p.SysName. The sysDescr must be the real one --
		// that is the vendor identity the collector's quirk matching keys off,
		// and inventing it is the mistake this whole profile mechanism exists
		// to prevent. The sysName is the opposite case: it names a *device*,
		// and defaulting it to the profile's sample means synthetic traffic
		// lands in the same router_sysname as the real lab node the sample was
		// taken from. Observed doing exactly that -- a bmpgen run once wrote
		// 480 rows under a real lab device's own hostname.
		//
		// So: real vendor, obviously-synthetic device name. An operator can
		// still override it with -router if they want a specific name.
		*router = defaultSysName(p)
	}
	if *vendor == "" {
		*vendor = p.SysDescr
	}

	conn, err := net.Dial("tcp", *target)
	if err != nil {
		return fmt.Errorf("dial %s: %w", *target, err)
	}
	defer conn.Close()
	msgs := genMessages(*router, *vendor, *peers, *updates)
	dumpLen := len(msgs)
	if *churnPrefixes > 0 && *churnRounds > 0 {
		fams, err := parseFamilies(*families)
		if err != nil {
			return err
		}
		for i := 0; i < *peers; i++ {
			peerIP := netip.AddrFrom4([4]byte{10, 0, 0, byte(9 + i)})
			ph := bmp.PeerHeader{
				Type: bmp.PeerTypeGlobal, Addr: peerIP, AS: uint32(65001 + i),
				BGPID: peerIP.String(), Timestamp: time.Now(),
			}
			for _, f := range fams {
				if f == bgp.FamilyIPv4U {
					msgs = append(msgs, genChurn(ph, *churnPrefixes, *churnRounds)...)
					continue
				}
				msgs = append(msgs, genChurnFamily(ph, f, *churnPrefixes, *churnRounds)...)
			}
		}
	}
	if err := sendMessages(conn, msgs, dumpLen, *churnInterval, time.Sleep); err != nil {
		return fmt.Errorf("write to %s: %w", *target, err)
	}
	fmt.Fprintf(os.Stderr, "bmpgen: sent %d messages to %s; holding session open (ctrl-c to close)\n",
		len(msgs), *target)
	// BMP sessions are long-lived (hours); exiting immediately here would
	// close the TCP connection right after the initial dump, and the
	// collector would treat the next bmpgen invocation as a brand-new
	// session that re-sends Initiation/Peer-Up/full-RIB from scratch rather
	// than exercising a steady-state feed. Holding the socket open until the
	// operator asks to stop is the realistic behavior.
	//
	// Passing nil as NotifyContext's parent panics ("cannot create context
	// from nil parent"); context.Background() avoids that.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
	return nil
}
