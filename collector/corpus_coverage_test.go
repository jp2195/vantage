package collector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// TestCorpusFeatureCoverage answers one question: which product features does
// the committed corpus of real vendor captures actually carry an example of?
//
// It exists because real vendor bytes are scarce: the only ones this project
// has are those in bgp/testdata/corpus. A synthetic generator can produce
// volume and churn, but it can only ever emit bytes we wrote, which tests the
// decoder against its own beliefs rather than against a shipping
// implementation. (Vendor anchors written from documentation, for example,
// match no real sender, which is why quirk.anchors is built from captured
// banners.)
//
// So this walks every capture, tallies which features appear, and reports the
// features with no example at all. Its output is the list of captures still
// worth taking -- and, when that list is empty for a feature, the evidence
// that no further capture is needed for it.
//
// It is deliberately a report and not an assertion: the point is to make the
// holes visible and measured, rather than to freeze today's accidental
// coverage as the contract.
func TestCorpusFeatureCoverage(t *testing.T) {
	root := filepath.Join("..", "bgp", "testdata", "corpus")
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".bmpcap") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Skipf("corpus tree not present: %v", err)
	}
	if len(files) == 0 {
		t.Skip("no corpus captures committed")
	}
	sort.Strings(files)

	// feature -> set of fixtures demonstrating it.
	cov := map[string]map[string]bool{}
	see := func(feature, fixture string) {
		if cov[feature] == nil {
			cov[feature] = map[string]bool{}
		}
		cov[feature][fixture] = true
	}

	for _, path := range files {
		fixture := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+"/")
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("read %s: %v", path, rerr)
			continue
		}
		scanRawFeatures(t, raw, fixture, see)
		scanDecodedFeatures(t, raw, fixture, see)
	}

	// The feature axes worth having a real example of, in the order a reader
	// would want them. Every name here is a thing the pipeline claims to
	// handle; a zero-coverage row is a claim no real router has ever backed.
	axes := []struct {
		name  string
		feats []string
	}{
		{"BMP message types", []string{
			"bmp/initiation", "bmp/peer-up", "bmp/peer-down", "bmp/route-monitoring",
			"bmp/stats-report", "bmp/termination", "bmp/route-mirroring",
		}},
		{"RIB streams (per-peer header flags)", []string{
			"rib/adj-rib-in-pre", "rib/adj-rib-in-post", "rib/adj-rib-out-pre",
			"rib/adj-rib-out-post", "rib/loc-rib", "rib/peer-ipv6", "rib/peer-2byte-aspath",
			// A PE's VRF peers are reported with peer type 1 and the VRF's RD
			// in the peer distinguisher, not with the type 0 / all-zeros
			// header every other row here exercises. peerheader.go has
			// decoded this since it was written; nothing proved a real router
			// emits it, which is the same "we handle it" claim with no bytes
			// behind it that family/vpn6 turned out to be.
			"rib/peer-rd-instance",
			// ADD-PATH negotiated in the Peer Up OPENs. Distinct from
			// route/addpath-pathid below: this is the capability, that is a
			// non-zero Path ID actually arriving on the wire.
			"rib/peer-addpath",
		}},
		{"Address families", []string{
			"family/ipv4u", "family/ipv6u", "family/lu4", "family/vpn4",
			"family/vpn6", "family/evpn", "family/ls",
		}},
		{"Path attributes", []string{
			"attr/origin", "attr/as-path", "attr/next-hop", "attr/med", "attr/local-pref",
			"attr/atomic-aggregate", "attr/aggregator", "attr/communities",
			"attr/extended-communities", "attr/large-communities", "attr/unknown",
			// RFC 5549: IPv4 NLRI reached via an IPv6 next hop, which arrives
			// as a 16- or 32-byte next-hop field under AFI 1 where the decoder
			// would otherwise expect 4. A length-driven trap, and unicast IPv4
			// over an IPv6 core is how a real fabric is built.
			"attr/nexthop-v6-for-v4",
		}},
		{"Route events", []string{
			"route/announce", "route/withdraw", "route/end-of-rib",
			"route/vpn-announce", "route/vpn-withdraw",
			"route/evpn-announce", "route/evpn-withdraw",
			// A non-zero RFC 7911 Path ID decoded off the wire. This is the
			// row that matters most in this block: with no ADD-PATH capability
			// in scope, prefix.go GUESSES whether an NLRI carries a Path ID
			// (ParseFlag_ADDPATH_HEURISTIC), and no capture has ever put a
			// real one in front of it.
			"route/addpath-pathid",
		}},
		// ASN width, which is two separate things and both must be covered:
		// how the AS_PATH is ENCODED on the wire (the per-peer header's A
		// flag, in the RIB-streams block above) and the VALUE of the ASNs it
		// carries. A corpus made entirely of ASNs below 65536 never exercises
		// the 4-byte path even when every session negotiated 4-byte AS,
		// because the values still fit in 16 bits.
		{"ASN width", []string{
			"asn/2byte-value", "asn/4byte-value",
			"asn/aggregator-2byte", "asn/aggregator-4byte",
		}},
		{"BGP-LS", []string{"ls/nodes", "ls/links", "ls/prefixes"}},
		{"Peer events", []string{"peer/up", "peer/down"}},
		// The target fleet, and the only vendors in scope: NX-OS and IOS-XR
		// are the production platforms, IOS-XE is the third Cisco train here,
		// and FRR is the one implementation available that does BMP fully
		// (it is the only source of post-policy and route-mirroring bytes).
		//
		// Arista, Junos and SR Linux are deliberately absent. Arista and
		// Junos images are not obtainable; SR Linux ships BMP gated behind
		// `if-feature bgp-bmp-support`, off on every platform in the free
		// image that boots (tested 2026-08-17 against SR Linux 26.7.1-554,
		// where `protocols bgp` has no bmp node at all). Listing them
		// as MISSING forever would bury the gaps that can actually be closed.
		// vendor/iosxe is MISSING even though the corpus now holds IOS-XE
		// captures, and that is the finding rather than a gap to close.
		// Cat8000v 17.18.02 sends a 6-byte Initiation with ZERO TLVs -- no
		// sysDescr, no sysName -- so there is no string for quirk.anchors or
		// bmptest.Profiles() to key on. A sender that identifies itself with
		// nothing is a blind spot in identification-by-sysDescr, and inventing
		// an anchor for it would break the rule quirk.anchors follows: no
		// sample, no anchor. The per-vendor matrix below still scores IOS-XE
		// correctly, because it attributes a capture by its directory.
		{"Vendors (Initiation sysDescr)", []string{
			"vendor/iosxr", "vendor/nxos", "vendor/frr", "vendor/iosxe",
		}},
	}

	// The target fleet, ordered as the matrices below print them. Declared
	// here rather than beside the matrix because the discovered-dimension
	// report above needs the same list.
	vendors := []string{"iosxr", "nxos", "frr", "iosxe"}

	var missing []string
	var report strings.Builder
	fmt.Fprintf(&report, "\ncorpus feature coverage over %d captures\n", len(files))
	for _, ax := range axes {
		fmt.Fprintf(&report, "\n  %s\n", ax.name)
		for _, f := range ax.feats {
			n := len(cov[f])
			if n == 0 {
				missing = append(missing, f)
				fmt.Fprintf(&report, "    %-32s  MISSING\n", f)
				continue
			}
			ex := sortedKeys(cov[f])
			fmt.Fprintf(&report, "    %-32s  %d  (%s)\n", f, n, ex[0])
		}
	}

	// Anything observed that no axis above names -- ext-community subtypes,
	// EVPN route types, stat counters, parse flags. These are discovered from
	// the bytes rather than enumerated, so they are reported as a list rather
	// than checked against an expected set.
	for _, prefix := range []string{
		"family-raw/", "extcomm/", "extcomm-raw/", "evpn-type/", "stat/", "parse-flag/",
		"peer-down-reason/", "peer-type/", "peer-flags/", "attr-code/", "aspath-seg/",
		"nh/", "rd-type/", "evpn-rd-type/", "label-stack/", "ls-proto/", "ls-routerid-len/",
		"ls-prefix-bytes/", "ls-ospf-route-type/", "ls-tlv-unknown/", "ls-attr-tlv-unknown/",
		"init-tlv/", "peerup-tlv/", "term-tlv/", "term-reason/", "cap-sent/", "cap-rcvd/",
	} {
		var got []string
		for f := range cov {
			if rest, ok := strings.CutPrefix(f, prefix); ok {
				got = append(got, rest)
			}
		}
		sortTokens(got)
		fmt.Fprintf(&report, "\n  observed %-22s %s\n", strings.TrimSuffix(prefix, "/")+":", strings.Join(got, " "))
	}

	// The same discovered dimensions again, split by vendor. The list above
	// answers "has any router ever sent this"; this answers "has THIS one",
	// and the two differ far more than the named per-vendor matrix suggests.
	// Measured examples, all invisible in the global list: NX-OS emits
	// exactly one BMP statistics counter where IOS-XE emits seventeen; EVPN
	// route types 2 and 5 exist only in NX-OS captures, so IOS-XR's and
	// IOS-XE's encoding of the two most complex EVPN types is unproven; and
	// every OSPF/PE extended community comes from IOS-XR alone.
	//
	// A dimension nobody can enumerate in advance cannot be a MISSING row --
	// there is no denominator -- so this stays a report. Its job is to make a
	// one-vendor column impossible to read as parity.
	fmt.Fprintf(&report, "\n  discovered dimensions by vendor (a value on one vendor only is not parity)\n")
	for _, prefix := range []string{
		"nh/", "stat/", "peer-down-reason/", "peer-type/", "evpn-type/", "extcomm-raw/",
		"aspath-seg/", "rd-type/", "ls-proto/", "ls-ospf-route-type/", "cap-sent/",
		"attr-code/", "term-reason/",
	} {
		var vals []string
		for f := range cov {
			if rest, ok := strings.CutPrefix(f, prefix); ok {
				vals = append(vals, rest)
			}
		}
		sortTokens(vals)
		fmt.Fprintf(&report, "    %s\n", strings.TrimSuffix(prefix, "/"))
		for _, val := range vals {
			cells := make([]string, 0, len(vendors))
			for _, v := range vendors {
				hit := "  -  "
				for fx := range cov[prefix+val] {
					if strings.HasPrefix(fx, v+"/") {
						hit = "  y  "
						break
					}
				}
				cells = append(cells, hit)
			}
			fmt.Fprintf(&report, "      %-30s %s\n", val, strings.Join(cells, ""))
		}
	}

	// Features a vendor's BMP implementation cannot produce, each with the
	// measurement that established it. Without this the per-vendor matrix
	// cannot say whether a blank cell is work to do or a dead end, and
	// "finish vendor X" has no definition -- every vendor would sit forever
	// at some number of blanks that nobody can close.
	//
	// Everything here was established by asking the device, not by reading
	// documentation, which says what a box prints at its CLI and not what it
	// puts on the BMP wire.
	platformLimits := map[string]map[string]string{
		// NX-OS and IOS-XR both send a Termination, and the corpus proves it:
		// n9kv-10.6.2F-termination.bmpcap carries TLV 0 "Administratively
		// Down" and xrd-26.1.1-p2-evpn-withdraw.bmpcap carries TLV 0 "config
		// shutdown", each followed by a Reason TLV of 0. So neither has a
		// termination row here: a wrong entry in THIS table is the expensive
		// kind, because it is the one place that says "stop looking".
		"nxos": {
			"bmp/route-mirroring":   "emits no BMP type 6",
			"rib/adj-rib-in-post":   "bmp-activate-server has no policy option",
			"rib/adj-rib-out-pre":   "no RFC 8671 O-flag support",
			"rib/adj-rib-out-post":  "no RFC 8671 O-flag support",
			"rib/loc-rib":           "no BMP RIB selection at all",
			"rib/peer-2byte-aspath": "needs a peer that declines 4-byte AS",
			"family/lu4":            "no ipv4 labeled-unicast AF exists (address-family ipv4 ? offers only multicast/mvpn/unicast)",
		},
		"iosxr": {
			"rib/adj-rib-out-pre":  "no RFC 8671 O-flag support",
			"rib/adj-rib-out-post": "no RFC 8671 O-flag support",
			"rib/adj-rib-in-post":  "bmp-activate ? offers only 'server'",
			"bmp/route-mirroring":  "emits no BMP type 6",
			// Asked both places a RIB could be selected on XRd 26.1.1:
			// `bmp-activate ?` under a neighbor offers only `server`, and the
			// `bmp server` submode offers only transport and timing knobs
			// (host, update-source, vrf, dscp, tcp, initial-delay,
			// initial-refresh, stats-reporting-period, flapping-delay,
			// shutdown). There is no RIB selection anywhere in XR's BMP
			// configuration, so RFC 9069 Loc-RIB cannot be produced.
			"rib/loc-rib": "no BMP RIB selection exists in XR's BMP config at all",
		},
		"frr": {
			"rib/adj-rib-out-pre":  "bmp monitor offers only pre-policy/post-policy/loc-rib",
			"rib/adj-rib-out-post": "bmp monitor offers only pre-policy/post-policy/loc-rib",
			// FRR has no BGP-LS implementation at all, so these are not
			// captures anyone can go and take. Measured by asking the daemon
			// rather than by reading release notes: under `router bgp`,
			// `address-family ?` offers only ipv4, ipv6 and l2vpn, and their
			// modifiers are unicast / multicast / labeled-unicast / vpn /
			// flowspec and evpn. There is no link-state family to activate.
			"family/ls":   "no link-state address family exists (address-family ? offers only ipv4/ipv6/l2vpn)",
			"ls/nodes":    "no link-state address family exists",
			"ls/links":    "no link-state address family exists",
			"ls/prefixes": "no link-state address family exists",
			// Measured by removing `bmp targets` with a mirror armed: the
			// session closes cleanly and not one further byte is mirrored.
			// IOS-XE behaves the same way, so only NX-OS and IOS-XR emit a
			// Termination in this corpus -- worth knowing before treating
			// type 5 as an end-of-session signal.
			"bmp/termination": "closes the session without sending type 5",
			// Measured 2026-08-27: `dont-capability-negotiate` genuinely
			// suppresses the capability -- `show bgp neighbor` drops from
			// "4 Byte AS: advertised and received" to "advertised" -- and FRR
			// still emits A=0 in every per-peer header, because it re-encodes
			// the AS_PATH as 4-byte internally and the flag describes the
			// mirrored message rather than the negotiated capability. IOS-XE
			// was measured doing the same thing. That is two of the four
			// senders behaving this way, and IOS-XR the only one that does not.
			"rib/peer-2byte-aspath": "sets A=0 even when the peer declines 4-byte AS (re-encodes internally)",
		},
		// IOS-XE has no blanket "*" row, although it can look as if BMP is
		// accepted and inert there. Without `update-source` under
		// `bmp server` the server is created and shows CfgSvr# 1, ActSvr#
		// stays empty, TCB is 0x0, and the router never opens the TCP
		// connection -- indistinguishable from an unsupported platform
		// unless you know to look for the one knob. With it, Cat8000v
		// 17.18.02 connects immediately and sends Peer Up, Peer Down and
		// Route Monitoring. A "*" row would excuse every IOS-XE cell and
		// report the vendor COMPLETE while it contributed zero bytes.
		//
		// Only the rows below are real IOS-XE limits. Everything else is left
		// OPEN on purpose: unmeasured is not the same as unobtainable, and
		// this table is the one place that distinction is enforced.
		"iosxe": {
			// Measured 2026-08-28 by enumerating the submode rather than
			// assuming: `neighbor X bmp-activate ?` on Cat8000v 17.18.02
			// offers only `all` and `server`. There is no policy or RIB
			// selection, which puts IOS-XE in exactly the same position as
			// IOS-XR and NX-OS on these four rows.
			"rib/adj-rib-in-post":  "bmp-activate ? offers only 'all' and 'server'",
			"rib/adj-rib-out-pre":  "bmp-activate ? offers only 'all' and 'server'",
			"rib/adj-rib-out-post": "bmp-activate ? offers only 'all' and 'server'",
			"rib/loc-rib":          "no BMP RIB selection exists in IOS-XE's BMP config at all",
			// Measured by deactivating the server with a mirror armed: the
			// capture holds Initiation, Peer Up, Route Monitoring and stats,
			// and no type 5, while the collector logs a clean close rather
			// than a reset. So IOS-XE behaves like FRR here, and unlike
			// NX-OS and IOS-XR, which both send one.
			"bmp/termination": "closes the TCP session cleanly without sending type 5",
			// Measured, and the measurement is stronger than the one behind
			// the NX-OS row: the peer really did decline. A lab XR router
			// was given `capability suppress 4-byte-as`, the session came
			// up reporting "Four-octets ASN Capability: advertised" with no
			// "and received", and every per-peer header in the resulting capture
			// still carried A=0. So IOS-XE behaves like FRR -- it re-encodes
			// the AS_PATH internally and reports the encoding it uses, not
			// the one it negotiated. IOS-XR is so far the only sender that
			// sets this flag honestly.
			"rib/peer-2byte-aspath": "sets A=0 even when the peer declines 4-byte AS (re-encodes internally, like FRR)",
			// Asked in all three places the knob could live on Cat8000v
			// 17.18.02: `bmp ?` under router bgp offers buffer-size,
			// initial-refresh and server; the `bmp server` submode offers
			// only transport and timing; and `neighbor X bmp-activate ?`
			// offers only all and server. There is nowhere to turn mirroring
			// on, so IOS-XE joins IOS-XR and NX-OS here and FRR remains the
			// only source of BMP type 6 in this corpus.
			"bmp/route-mirroring": "no mirroring option exists anywhere in IOS-XE's BMP config",
			// IOS-XE accepts IOS-XR's BGP-LS once the IGP is IS-IS, so there
			// is no row for it here. With OSPF as the only IGP distributing,
			// activating the family drew NOTIFICATION 3/1 (malformed
			// attribute list) and took IPv4 and IPv6 down with it every
			// time. With the XR nodes on IS-IS, the same address family on
			// the same session is accepted: 26 prefixes received, session up,
			// and not one NOTIFICATION or MSGDUMP in `show logging`. See
			// iosxe/cat8000v-17.18.02-rr-isis-linkstate.bmpcap, which carries
			// nodes, links and prefixes.
			//
			// So the rejection was of something OSPF-shaped in IOS-XR's
			// encoding, not of BGP-LS as such, and a row here would say
			// "stop looking" about something that is obtainable.
		},
	}
	limited := func(vendor, feat string) (string, bool) {
		m := platformLimits[vendor]
		if m == nil {
			return "", false
		}
		if r, ok := m["*"]; ok {
			return r, true
		}
		r, ok := m[feat]
		return r, ok
	}

	// Per-vendor matrix. A feature covered only by IOS-XR is NOT covered for
	// NX-OS: the two implementations disagree about enough (next-hop
	// encodings, which attributes they originate, what they put in
	// Initiation) that one vendor's bytes cannot stand in for another's.
	// vantage's production fleet is NX-OS first, so an XR-only row is a real
	// gap, not a rounding error.
	fmt.Fprintf(&report, "\n  per-vendor coverage (a row covered by only one vendor is not parity)\n")
	fmt.Fprintf(&report, "    %-32s %s\n", "feature", strings.Join(vendors, "  "))
	for _, ax := range axes {
		if strings.HasPrefix(ax.name, "Vendors") {
			continue
		}
		for _, f := range ax.feats {
			cells := make([]string, 0, len(vendors))
			for _, v := range vendors {
				hit := "  -  "
				for fx := range cov[f] {
					if strings.HasPrefix(fx, v+"/") {
						hit = "  y  "
						break
					}
				}
				if hit == "  -  " {
					if _, isLimited := limited(v, f); isLimited {
						hit = " n/a "
					}
				}
				cells = append(cells, hit)
			}
			fmt.Fprintf(&report, "    %-32s %s\n", f, strings.Join(cells, ""))
		}
	}

	if len(missing) > 0 {
		fmt.Fprintf(&report, "\n  %d features with NO real-router example:\n    %s\n",
			len(missing), strings.Join(missing, "\n    "))
	}

	// Per-vendor completion. A vendor is DONE when every feature it is
	// physically capable of producing has a capture -- not when it has no
	// blank cells, which is unreachable for all of them.
	fmt.Fprintf(&report, "\n  per-vendor completion\n")
	for _, v := range vendors {
		var covered, na, open []string
		for _, ax := range axes {
			if strings.HasPrefix(ax.name, "Vendors") {
				continue
			}
			for _, f := range ax.feats {
				got := false
				for fx := range cov[f] {
					if strings.HasPrefix(fx, v+"/") {
						got = true
						break
					}
				}
				switch {
				case got:
					covered = append(covered, f)
				default:
					if _, isLimited := limited(v, f); isLimited {
						na = append(na, f)
					} else {
						open = append(open, f)
					}
				}
			}
		}
		status := fmt.Sprintf("%d covered, %d n/a, %d OPEN", len(covered), len(na), len(open))
		switch {
		case len(covered) == 0:
			// Everything n/a and nothing covered is not completion, it is a
			// vendor that cannot be captured at all. Saying COMPLETE here
			// would turn a total blocker into a green tick.
			status += "  <- BLOCKED (nothing obtainable)"
		case len(open) == 0:
			status += "  <- COMPLETE"
		}
		fmt.Fprintf(&report, "    %-8s %s\n", v, status)
		if len(open) > 0 {
			fmt.Fprintf(&report, "             open: %s\n", strings.Join(open, " "))
		}
	}
	t.Log(report.String())
}

// scanRawFeatures reads the capture at the BMP framing level, before any
// decoding, so it can see things the Session layer normalizes away: which
// message types are present at all, the per-peer header flag combinations
// (which is the only way to tell adj-RIB-out and Loc-RIB apart from ordinary
// adj-RIB-in), and the Initiation sysDescr that identifies the sender.
func scanRawFeatures(t *testing.T, raw []byte, fixture string, see func(string, string)) {
	t.Helper()
	r := bytes.NewReader(raw)
	for {
		m, err := bmp.ReadMsg(r)
		if err != nil {
			return
		}
		switch m.Type {
		case bmp.TypeInitiation:
			see("bmp/initiation", fixture)
			seeVendor(m.Payload, fixture, see)
			seeTLVTypes(m.Payload, "init-tlv/", fixture, see)
			continue
		case bmp.TypeTermination:
			see("bmp/termination", fixture)
			seeTLVTypes(m.Payload, "term-tlv/", fixture, see)
			// RFC 7854 4.5: the Reason TLV (type 1) carries a 2-byte code.
			// Recorded separately from the TLV type because "a Termination
			// arrived" and "the router said WHY" are different claims, and
			// only two reasons out of five have ever been on a wire here.
			eachTLV(m.Payload, func(typ uint16, v []byte) {
				if typ == 1 && len(v) == 2 {
					see(fmt.Sprintf("term-reason/%d", binary.BigEndian.Uint16(v)), fixture)
				}
			})
			continue
		case bmp.TypeRouteMonitoring:
			see("bmp/route-monitoring", fixture)
		case bmp.TypeStatsReport:
			see("bmp/stats-report", fixture)
		case bmp.TypePeerDown:
			see("bmp/peer-down", fixture)
		case bmp.TypePeerUp:
			see("bmp/peer-up", fixture)
		case bmp.TypeRouteMirroring:
			see("bmp/route-mirroring", fixture)
		}
		// Everything that reaches here carries a per-peer header.
		ph, rest, err := bmp.ParsePeerHeader(m.Payload)
		if err != nil {
			continue
		}
		// The peer type and the whole flag byte, recorded raw. The named
		// rib/* rows above collapse these into "which stream is this", which
		// is the right question for coverage and the wrong one for finding
		// out what a vendor never emits: peer type 2 (Local Instance) and
		// the V+A flag combination are both absent from every capture here,
		// and neither absence is visible in a row that only asks "is this
		// adj-RIB-in".
		see(fmt.Sprintf("peer-type/%d", ph.Type), fixture)
		see(fmt.Sprintf("peer-flags/0x%02x", ph.Flags), fixture)
		switch m.Type {
		case bmp.TypePeerUp:
			seePeerUp(rest, fixture, see)
		case bmp.TypeRouteMonitoring:
			seeRawUpdate(rest, fixture, see)
		}
		switch {
		case ph.Type == bmp.PeerTypeLocRIB:
			see("rib/loc-rib", fixture)
		case ph.AdjRIBOut() && ph.PostPolicy():
			see("rib/adj-rib-out-post", fixture)
		case ph.AdjRIBOut():
			see("rib/adj-rib-out-pre", fixture)
		case ph.PostPolicy():
			see("rib/adj-rib-in-post", fixture)
		default:
			see("rib/adj-rib-in-pre", fixture)
		}
		if ph.IPv6() {
			see("rib/peer-ipv6", fixture)
		}
		if ph.TwoByteASPath() {
			see("rib/peer-2byte-aspath", fixture)
		}
		// Both halves are required. Type 1 says "this peer belongs to a
		// routing instance"; the distinguisher says which one. A type 1
		// header with an all-zero distinguisher would prove nothing about
		// the field the decoder has to get right.
		if ph.Type == bmp.PeerTypeRD && ph.Distinguisher != 0 {
			see("rib/peer-rd-instance", fixture)
		}
	}
}

// seeVendor records which implementation sent a capture, keyed off the
// Initiation sysDescr TLV -- the real strings off committed captures.
// The three matched here are the ones measured off committed captures
// (FRRouting 10.3_git / bare "26.1.1" for XRd / the Nexus9000 chassis line);
// the rest are listed so their absence stays visible.
func seeVendor(payload []byte, fixture string, see func(string, string)) {
	var descr string
	for len(payload) >= 4 {
		typ := int(payload[0])<<8 | int(payload[1])
		l := int(payload[2])<<8 | int(payload[3])
		if 4+l > len(payload) {
			return
		}
		if typ == 1 {
			descr = string(payload[4 : 4+l])
		}
		payload = payload[4+l:]
	}
	switch {
	case descr == "":
		return
	case strings.Contains(descr, "FRRouting"):
		see("vendor/frr", fixture)
	case strings.Contains(descr, "Nexus"):
		see("vendor/nxos", fixture)
	case strings.Contains(descr, "IOS-XE"), strings.Contains(descr, "IOS Software"):
		see("vendor/iosxe", fixture)
	case strings.Contains(descr, "EOS"):
		see("vendor/arista", fixture)
	case strings.Contains(descr, "JUNOS"):
		see("vendor/junos", fixture)
	case strings.Contains(descr, "SRLinux"), strings.Contains(descr, "SR Linux"):
		see("vendor/srlinux", fixture)
	default:
		// XRd sends a bare version with no vendor token at all -- "26.1.1".
		// Matching that shape is the only way to attribute it, and why no
		// anchor keyed on a vendor token can identify it.
		if len(descr) > 0 && descr[0] >= '0' && descr[0] <= '9' {
			see("vendor/iosxr", fixture)
		}
	}
}

// scanDecodedFeatures replays the capture through a Session and tallies what
// the pipeline actually produced -- families, attributes, route/peer/stats
// event shapes, and the parse flags raised along the way.
func scanDecodedFeatures(t *testing.T, raw []byte, fixture string, see func(string, string)) {
	t.Helper()
	t0 := time.Unix(0, 0)
	s := NewSession(netip.MustParseAddr("10.0.0.1"), "c1", 1,
		func() time.Time { return t0 }, Overrides{})

	r := bytes.NewReader(raw)
	for {
		m, err := bmp.ReadMsg(r)
		if err != nil {
			if err != io.EOF {
				t.Logf("%s: stopped decoding: %v", fixture, err)
			}
			break
		}
		for _, ev := range s.Handle(m) {
			env := ev.Env
			for _, fl := range env.GetParseFlags() {
				see("parse-flag/"+strings.TrimPrefix(fl.String(), "PARSE_FLAG_"), fixture)
			}
			if pe := env.GetPeerEvent(); pe != nil {
				switch pe.GetKind() {
				case vantagev1.PeerEvent_KIND_UP:
					see("peer/up", fixture)
					if len(pe.GetCaps().GetAddpathFamilies()) > 0 {
						see("rib/peer-addpath", fixture)
					}
				case vantagev1.PeerEvent_KIND_DOWN:
					see("peer/down", fixture)
					see(fmt.Sprintf("peer-down-reason/%d", pe.GetDownReason()), fixture)
				}
			}
			if st := env.GetStats(); st != nil {
				for k := range st.GetCounters() {
					see(fmt.Sprintf("stat/%d", k), fixture)
				}
			}
			if ls := env.GetLs(); ls != nil {
				// The source protocol every LS object was learned from, and
				// the width of its node descriptor's router-id. Both exist
				// because ls/nodes, ls/links and ls/prefixes are blind to the
				// difference that matters: an IS-IS node is identified by a
				// 6- or 7-byte ISO system ID where an OSPF node uses a 4-byte
				// router-id plus an area, so a corpus that is entirely
				// protocol 3 exercises one of two encodings while the report
				// shows three green rows.
				for _, n := range ls.GetNodes() {
					see(fmt.Sprintf("ls-proto/%d", n.GetProtocol()), fixture)
					see(fmt.Sprintf("ls-routerid-len/%d", len(n.GetLocal().GetRouterId())), fixture)
					for k := range n.GetUnknownTlvs() {
						see(fmt.Sprintf("ls-tlv-unknown/%d", k), fixture)
					}
				}
				for _, l := range ls.GetLinks() {
					see(fmt.Sprintf("ls-proto/%d", l.GetProtocol()), fixture)
					for k := range l.GetUnknownTlvs() {
						see(fmt.Sprintf("ls-tlv-unknown/%d", k), fixture)
					}
				}
				for _, pfx := range ls.GetPrefixes() {
					see(fmt.Sprintf("ls-proto/%d", pfx.GetProtocol()), fixture)
					// 4 bytes is an IPv4 Topology Prefix (NLRI type 3), 16
					// an IPv6 one (type 4). A 16-byte value now appears in
					// the corpus: it first did, on 2026-09-01, when type 4
					// was decoded against 74 captured NLRI. Both widths are
					// now expected.
					see(fmt.Sprintf("ls-prefix-bytes/%d", len(pfx.GetPrefix())), fixture)
					// RFC 9552 TLV 264: an OSPF Route Type is OSPF data, so
					// only a network running OSPF can produce it. 1 intra-area,
					// 2 inter-area, 3/4 external 1 and 2, 5/6 NSSA 1 and 2.
					// Only intra-area has ever arrived, which means one area,
					// no redistribution and no NSSA -- three configuration
					// facts about the network captures were taken from,
					// reported as if they were the shape of the protocol.
					see(fmt.Sprintf("ls-ospf-route-type/%d", pfx.GetOspfRouteType()), fixture)
					for k := range pfx.GetUnknownTlvs() {
						see(fmt.Sprintf("ls-tlv-unknown/%d", k), fixture)
					}
				}
				for k := range ls.GetUnknownTlvs() {
					see(fmt.Sprintf("ls-attr-tlv-unknown/%d", k), fixture)
				}
				if len(ls.GetNodes()) > 0 {
					see("ls/nodes", fixture)
					see("family/ls", fixture)
				}
				if len(ls.GetLinks()) > 0 {
					see("ls/links", fixture)
					see("family/ls", fixture)
				}
				if len(ls.GetPrefixes()) > 0 {
					see("ls/prefixes", fixture)
					see("family/ls", fixture)
				}
			}
			rt := env.GetRoute()
			if rt == nil {
				continue
			}
			// "The family appeared" and "the family was decoded" are
			// different claims, and conflating them is how a coverage report
			// lies. A family with no decodeNLRI entry still reaches here with
			// rt.Family set -- update.go records Family alongside RawReach on
			// the undecoded path -- so counting Family alone would report
			// vpn6 as covered when every vpn6 NLRI in the corpus is in fact
			// raw bytes carrying PARSE_FLAG_UNKNOWN_FAMILY. Split the two:
			// family/X means typed NLRI came out, family-raw/X means the
			// bytes are present and undecoded.
			tok := familyToken(rt.GetFamily())
			typed := len(rt.GetAnnounced()) > 0 || len(rt.GetWithdrawn()) > 0 ||
				len(rt.GetVpnAnnounced()) > 0 || len(rt.GetVpnWithdrawn()) > 0 ||
				len(rt.GetEvpnAnnounced()) > 0 || len(rt.GetEvpnWithdrawn()) > 0
			switch {
			case typed:
				see("family/"+tok, fixture)
			case len(rt.GetRawReach()) > 0 || len(rt.GetRawUnreach()) > 0:
				see("family-raw/"+tok, fixture)
			}
			if len(rt.GetAnnounced()) > 0 {
				see("route/announce", fixture)
			}
			if len(rt.GetWithdrawn()) > 0 {
				see("route/withdraw", fixture)
			}
			if rt.GetEndOfRib() {
				see("route/end-of-rib", fixture)
			}
			// Path IDs ride on every NLRI shape, not just plain unicast, so
			// all four lists are checked rather than the two easy ones.
			for _, p := range rt.GetAnnounced() {
				if p.GetPathId() != 0 {
					see("route/addpath-pathid", fixture)
				}
			}
			for _, p := range rt.GetWithdrawn() {
				if p.GetPathId() != 0 {
					see("route/addpath-pathid", fixture)
				}
			}
			for _, p := range rt.GetVpnAnnounced() {
				if p.GetPathId() != 0 {
					see("route/addpath-pathid", fixture)
				}
			}
			for _, p := range rt.GetVpnWithdrawn() {
				if p.GetPathId() != 0 {
					see("route/addpath-pathid", fixture)
				}
			}
			for _, v := range append(rt.GetVpnAnnounced(), rt.GetVpnWithdrawn()...) {
				if t := rdType(v.GetRd()); t != "" {
					see("rd-type/"+t, fixture)
				}
				// A VPN or BGP-LU NLRI may carry a stack, and decodeLabels
				// walks up to maxLabelStack entries looking for the
				// bottom-of-stack bit. Every entry in this corpus is depth 1,
				// so that walk has never had to iterate.
				see(fmt.Sprintf("label-stack/%d", len(v.GetLabels())), fixture)
			}
			for _, e := range append(rt.GetEvpnAnnounced(), rt.GetEvpnWithdrawn()...) {
				if t := rdType(e.GetRd()); t != "" {
					see("evpn-rd-type/"+t, fixture)
				}
			}
			if len(rt.GetVpnAnnounced()) > 0 {
				see("route/vpn-announce", fixture)
			}
			if len(rt.GetVpnWithdrawn()) > 0 {
				see("route/vpn-withdraw", fixture)
			}
			if len(rt.GetEvpnAnnounced()) > 0 {
				see("route/evpn-announce", fixture)
			}
			if len(rt.GetEvpnWithdrawn()) > 0 {
				see("route/evpn-withdraw", fixture)
			}
			for _, e := range rt.GetEvpnAnnounced() {
				see(fmt.Sprintf("evpn-type/%d", e.GetRouteType()), fixture)
			}
			for _, e := range rt.GetEvpnWithdrawn() {
				see(fmt.Sprintf("evpn-type/%d", e.GetRouteType()), fixture)
			}
			a := rt.GetAttrs()
			if a == nil {
				continue
			}
			// Origin 0 (IGP) is both the zero value and a real wire value, so
			// presence of the attribute is inferred from the route carrying
			// any attributes at all rather than from a non-zero origin.
			see("attr/origin", fixture)
			if len(a.GetAsPath()) > 0 {
				see("attr/as-path", fixture)
				for _, seg := range a.GetAsPath() {
					// RFC 4271 1 AS_SET / 2 AS_SEQUENCE, RFC 5065 3
					// AS_CONFED_SEQUENCE / 4 AS_CONFED_SET. parseASPath
					// accepts all four; only one has ever arrived.
					see(fmt.Sprintf("aspath-seg/%d", seg.GetType()), fixture)
					for _, asn := range seg.GetAsns() {
						if asn > 65535 {
							see("asn/4byte-value", fixture)
						} else {
							see("asn/2byte-value", fixture)
						}
					}
				}
			}
			if a.GetNextHop() != "" {
				see("attr/next-hop", fixture)
				// RFC 5549 is "IPv4 reachability, IPv6 next hop", so the
				// interesting case is an IPv6 next hop attached to a v4
				// family -- decided on the parsed address rather than on the
				// text, so a v4-mapped form ("::ffff:10.0.0.1") cannot pass
				// as native IPv6.
				if nh, err := netip.ParseAddr(a.GetNextHop()); err == nil &&
					nh.Is6() && !nh.Is4In6() && isV4Family(rt.GetFamily()) {
					see("attr/nexthop-v6-for-v4", fixture)
				}
			}
			if a.Med != nil {
				see("attr/med", fixture)
			}
			if a.LocalPref != nil {
				see("attr/local-pref", fixture)
			}
			if a.GetAtomicAggregate() {
				see("attr/atomic-aggregate", fixture)
			}
			if agg := a.GetAggregator(); agg != nil {
				see("attr/aggregator", fixture)
				if agg.GetAsn() > 65535 {
					see("asn/aggregator-4byte", fixture)
				} else {
					see("asn/aggregator-2byte", fixture)
				}
			}
			if len(a.GetCommunities()) > 0 {
				see("attr/communities", fixture)
			}
			if len(a.GetLargeCommunities()) > 0 {
				see("attr/large-communities", fixture)
			}
			if len(a.GetUnknown()) > 0 {
				see("attr/unknown", fixture)
			}
			for _, ec := range a.GetExtendedCommunities() {
				see("attr/extended-communities", fixture)
				see("extcomm/"+bgp.ExtCommName(uint8(ec.GetType()), uint8(ec.GetSubType())), fixture)
				// The (type, subtype) pair as well as the name, because
				// ExtCommName returns "" for a pair it does not model and the
				// named axis therefore cannot distinguish "never arrived"
				// from "arrived and we have no name for it". 06:04 (RFC 8214
				// Layer-2 Attributes) is exactly that case: IOS-XR and
				// IOS-XE both send it and nothing here says so.
				see(fmt.Sprintf("extcomm-raw/%02x:%02x", ec.GetType(), ec.GetSubType()), fixture)
			}
		}
	}
}

// familyToken renders a Family the way the sink's family column would, so a
// coverage row reads with the same name the product uses elsewhere.
func familyToken(f *vantagev1.Family) string {
	afi, safi := f.GetAfi(), f.GetSafi()
	switch {
	case afi == 1 && safi == 1:
		return "ipv4u"
	case afi == 2 && safi == 1:
		return "ipv6u"
	case afi == 1 && safi == 4:
		return "lu4"
	case afi == 1 && safi == 128:
		return "vpn4"
	case afi == 2 && safi == 128:
		return "vpn6"
	case afi == 25 && safi == 70:
		return "evpn"
	case afi == 16388 && safi == 71:
		return "ls"
	}
	return fmt.Sprintf("x%d-%d", afi, safi)
}

// isV4Family reports whether f carries IPv4 reachability, which is AFI 1
// whatever the SAFI: plain unicast, labeled-unicast and VPN-IPv4 can all be
// advertised with an IPv6 next hop under RFC 5549.
func isV4Family(f *vantagev1.Family) bool { return f.GetAfi() == 1 }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// eachTLV walks a BMP TLV sequence -- 2-byte type, 2-byte length, value --
// stopping at the first header that does not fit. Initiation, Termination and
// Peer Up all carry the same shape, so they share one walk rather than three
// that can drift.
func eachTLV(b []byte, fn func(typ uint16, v []byte)) {
	for len(b) >= 4 {
		typ := binary.BigEndian.Uint16(b[0:2])
		l := int(binary.BigEndian.Uint16(b[2:4]))
		if 4+l > len(b) {
			return
		}
		fn(typ, b[4:4+l])
		b = b[4+l:]
	}
}

func seeTLVTypes(b []byte, prefix, fixture string, see func(string, string)) {
	eachTLV(b, func(typ uint16, _ []byte) {
		see(fmt.Sprintf("%s%d", prefix, typ), fixture)
	})
}

// seePeerUp reads a Peer Up's two OPEN messages and records the capability
// codes in each, plus any TLVs trailing them.
//
// Sent and received are kept apart deliberately. A Peer Up carries the
// monitored router's own OPEN first and its peer's second (RFC 7854 4.10), so
// only the sent half is evidence about the sender's implementation. Merging
// them would let a capability the FRR feeder advertised be read as one the
// Cisco router supports -- which is precisely the question
// attr/nexthop-v6-for-v4 turns on.
func seePeerUp(rest []byte, fixture string, see func(string, string)) {
	// local address(16) local port(2) remote port(2), then the two OPENs.
	if len(rest) < 20 {
		return
	}
	b := rest[20:]
	for i, dir := range []string{"cap-sent/", "cap-rcvd/"} {
		_ = i
		if len(b) < bgpHdrLen {
			return
		}
		ln := int(binary.BigEndian.Uint16(b[16:18]))
		if ln < bgpHdrLen || ln > len(b) {
			return
		}
		seeOpenCaps(b[bgpHdrLen:ln], dir, fixture, see)
		b = b[ln:]
	}
	// RFC 9069 4.1 adds a VRF/Table Name TLV (type 3) after the OPENs. Only
	// FRR has ever sent one; no Cisco platform here emits any Peer Up TLV.
	seeTLVTypes(b, "peerup-tlv/", fixture, see)
}

// bgpHdrLen is RFC 4271 4.1's 16-byte marker + 2-byte length + 1-byte type.
const bgpHdrLen = 19

func seeOpenCaps(op []byte, prefix, fixture string, see func(string, string)) {
	// version(1) my-as(2) hold(2) bgp-id(4) opt-param-len(1)
	if len(op) < 10 {
		return
	}
	optLen := int(op[9])
	opt := op[10:]
	if optLen > len(opt) {
		return
	}
	opt = opt[:optLen]
	for len(opt) >= 2 {
		pt, pl := opt[0], int(opt[1])
		if 2+pl > len(opt) {
			return
		}
		if pt == 2 { // RFC 5492 4: Capabilities Optional Parameter
			caps := opt[2 : 2+pl]
			for len(caps) >= 2 {
				code, cl := caps[0], int(caps[1])
				if 2+cl > len(caps) {
					break
				}
				see(fmt.Sprintf("%s%d", prefix, code), fixture)
				caps = caps[2+cl:]
			}
		}
		opt = opt[2+pl:]
	}
}

// seeRawUpdate reads the BGP UPDATE inside a Route Monitoring message at the
// wire level, before any decoding, and records two things the decoded pass
// cannot report: every path attribute TYPE CODE present (including the ones
// this build stores verbatim in Unknown, which the attr/unknown row counts
// without naming), and the MP_REACH next-hop LENGTH per family.
//
// The next-hop shape is the important half. nextHopForFamily claims sixteen
// (family, length) pairs across vpn4, vpn6, lu4, EVPN and BGP-LS -- including
// every RFC 8950 IPv6 form and every RFC 2545 link-local form -- and a
// decoded next hop is just a string, so nothing downstream can tell a 12-byte
// vpn4 next hop from a 24-byte one. Recording the length is the only way to
// see which of those claims a real router has ever backed.
func seeRawUpdate(rest []byte, fixture string, see func(string, string)) {
	if len(rest) < bgpHdrLen+4 {
		return
	}
	body := rest[bgpHdrLen:]
	wLen := int(binary.BigEndian.Uint16(body[0:2]))
	if 2+wLen+2 > len(body) {
		return
	}
	aLen := int(binary.BigEndian.Uint16(body[2+wLen : 4+wLen]))
	attrs := body[4+wLen:]
	if aLen > len(attrs) {
		return
	}
	attrs = attrs[:aLen]
	for len(attrs) >= 3 {
		flags, typ := attrs[0], attrs[1]
		var vLen, off int
		if flags&0x10 != 0 { // extended length
			if len(attrs) < 4 {
				return
			}
			vLen, off = int(binary.BigEndian.Uint16(attrs[2:4])), 4
		} else {
			vLen, off = int(attrs[2]), 3
		}
		if off+vLen > len(attrs) {
			return
		}
		v := attrs[off : off+vLen]
		see(fmt.Sprintf("attr-code/%d", typ), fixture)
		if typ == 14 && len(v) >= 4 { // MP_REACH_NLRI
			see(fmt.Sprintf("nh/%s-len%d",
				familyToken(&vantagev1.Family{
					Afi:  uint32(binary.BigEndian.Uint16(v[0:2])),
					Safi: uint32(v[2]),
				}), v[3]), fixture)
		}
		attrs = attrs[off+vLen:]
	}
}

// rdType infers a Route Distinguisher's RFC 4364 type from the text decodeRD
// produced, which is the only form the pipeline keeps. The inference is exact
// in one direction and lossy in the other: a type 1 always renders with a
// dotted administrator, and a type 2 whose 4-byte ASN exceeds 65535 cannot be
// confused with a type 0 -- but a type 2 carrying an ASN below 65536 renders
// identically to a type 0 and is counted as one. So this axis can UNDERCOUNT
// type 2 and can never overcount it, which is the safe direction for a report
// whose job is to find gaps. Real type 2 RDs exist because the ASN does not
// fit in two bytes, so the ambiguous case is theoretical.
func rdType(rd string) string {
	if rd == "" {
		return ""
	}
	i := strings.LastIndex(rd, ":")
	if i < 0 {
		return "malformed"
	}
	admin := rd[:i]
	if strings.Contains(admin, ".") {
		return "1-ipv4"
	}
	n, err := strconv.ParseUint(admin, 10, 32)
	if err != nil {
		return "malformed"
	}
	if n > 65535 {
		return "2-as4"
	}
	return "0-as2"
}

// sortTokens orders discovered values so a reader can scan them: numerically
// when every token is a number, lexically otherwise. Plain sort.Strings puts
// stat counter 11 between 1 and 2 and capability 128 before 2, which turns
// "which counters are missing" into a puzzle.
func sortTokens(v []string) {
	sort.Slice(v, func(i, j int) bool {
		a, aerr := strconv.Atoi(v[i])
		b, berr := strconv.Atoi(v[j])
		if aerr == nil && berr == nil {
			return a < b
		}
		return v[i] < v[j]
	})
}
