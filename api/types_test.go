package api

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/query"
	"gopkg.in/yaml.v3"
)

// mustMarshal marshals v compactly, so every assertion below can be written
// against the bytes a handler would actually write.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// keysOf marshals v and decodes it one level deep, so a test can ask which
// keys are on the wire without asserting anything about their values.
func keysOf(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshal(t, v), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// TestSessionIDMarshalsAsString guards a precision loss that no Go test
// would otherwise notice: Go's encoding/json round-trips uint64 exactly,
// so the bug only appears in a JavaScript client or an LLM's JSON parser,
// where 1774329600123456789 silently becomes 1774329600123456800.
func TestSessionIDMarshalsAsString(t *testing.T) {
	w := NewWireRouter(query.Router{SessionID: 1774329600123456789})
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"session_id":"1774329600123456789"`)) {
		t.Errorf("session_id must marshal as a quoted string; got:\n\t%s", b)
	}
}

// TestOriginASNIsNullWhenASPathIsEmpty pins the distinction a dashboard
// had to be fixed for on 2026-08-23: a route with no AS path has no origin,
// and reporting origin 0 for it is indistinguishable from a route that
// genuinely originated in the reserved AS 0.
//
// It is a live case, not a hypothetical one. Measured against a lab
// archive on 2026-08-30: 7 of 190 live unicast routes and 21 of 245 live
// VPN rows carry an empty path, and at the row level rather than the
// current-state one it is 1,839 of 8,611 unicast rows.
//
// The dates are on those numbers because they drift: an earlier archive
// put them at 36 of 420 unicast and 142 of 164 VPN, figures that no longer
// hold and are off by a factor of ten on the VPN side today. The claim
// the test makes is not a function of the archive; only the evidence
// that it matters is, so the evidence is stamped.
func TestOriginASNIsNullWhenASPathIsEmpty(t *testing.T) {
	b, err := json.Marshal(NewWireUnicastRoute(query.Route{ASPath: nil, OriginASN: 0}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"origin_asn":null`)) {
		t.Errorf("origin_asn must be null when as_path is empty; got:\n\t%s", b)
	}
}

// TestLabelZeroSurvivesMarshaling is the omitempty trap, and it is the
// reason label is a *uint32 rather than a uint32 with omitempty: 0 is the
// implicit-null label, a real value an MPLS router advertises, so dropping
// it and reporting absence are two different lies.
func TestLabelZeroSurvivesMarshaling(t *testing.T) {
	b, err := json.Marshal(NewWireVPNRoute(query.VPNRoute{Label: 0, HasLabel: true}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"label":0`)) {
		t.Errorf("a genuine implicit-null label 0 must survive; got:\n\t%s", b)
	}
	b, err = json.Marshal(NewWireVPNRoute(query.VPNRoute{Label: 0, HasLabel: false}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"label":null`)) {
		t.Errorf("no label must marshal as null, not 0; got:\n\t%s", b)
	}
}

// TestWarningsIsAlwaysAnArray keeps meta.warnings from marshaling as null
// on a nil slice. The contract documents an empty array as a positive claim
// -- "nothing was mid-dump" -- and a client distinguishing [] from null is
// exactly the kind of thing an LLM gets wrong.
func TestWarningsIsAlwaysAnArray(t *testing.T) {
	b, err := json.Marshal(Envelope{Data: []int{}, Meta: Meta{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"warnings":[]`)) {
		t.Errorf("warnings must be [] when empty, never null; got:\n\t%s", b)
	}
}

// TestWarningsCarryTheirEntries is the other half of the one above: a
// MarshalJSON that always returned [] would satisfy that test and lose
// every real warning.
func TestWarningsCarryTheirEntries(t *testing.T) {
	m := Meta{Warnings: []Warning{{Code: WarnSessionDumping, Message: "peer 10.0.103.61 is mid-dump"}}}
	b := mustMarshal(t, m)
	want := `"warnings":[{"code":"session_dumping","message":"peer 10.0.103.61 is mid-dump"}]`
	if !bytes.Contains(b, []byte(want)) {
		t.Errorf("warnings must carry their entries; got:\n\t%s", b)
	}
}

// TestDumpStatesIsAlwaysAnObject is TestWarningsIsAlwaysAnArray's sibling.
// query.Peer.DumpStates is empty for a DOWN peer -- deliberately, because a
// disconnected peer's dump progress is not a meaningful question -- so the
// nil-map case here is the ordinary shape of a down peer rather than an
// edge case, and null would tell a client "unknown" about the one state we
// are certain of.
func TestDumpStatesIsAlwaysAnObject(t *testing.T) {
	b := mustMarshal(t, NewWirePeer(query.Peer{State: "down", DumpStates: nil}))
	if !bytes.Contains(b, []byte(`"dump_states":{}`)) {
		t.Errorf("a down peer's dump_states must be {}, never null; got:\n\t%s", b)
	}
	b = mustMarshal(t, NewWirePeer(query.Peer{
		State:      "up",
		DumpStates: map[string]string{"ipv4u": "complete", "vpn4": "dumping"},
	}))
	if !bytes.Contains(b, []byte(`"dump_states":{"ipv4u":"complete","vpn4":"dumping"}`)) {
		t.Errorf("dump_states must carry its entries; got:\n\t%s", b)
	}
}

// TestNextHopIsNullNeverInvalidIP is hazard 2 in full. netip.Addr{}.String()
// returns the literal text "invalid IP", so a next hop that failed to parse
// -- or a withdrawal, which carries no path attributes and so has no next
// hop at all, the ORDINARY case on the history surface -- would otherwise
// reach a looking glass as a string that looks like an answer.
func TestNextHopIsNullNeverInvalidIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"unicast", NewWireUnicastRoute(query.Route{})},
		{"vpn", NewWireVPNRoute(query.VPNRoute{})},
		{"evpn", NewWireEVPNRoute(query.EVPNRoute{})},
		{"history", NewWireHistoryEvent(query.HistoryEvent{Action: "withdraw"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := mustMarshal(t, tc.v)
			if !bytes.Contains(b, []byte(`"next_hop":null`)) {
				t.Errorf("an invalid next hop must marshal as null; got:\n\t%s", b)
			}
			if bytes.Contains(b, []byte("invalid IP")) {
				t.Errorf("netip.Addr{}.String() leaked to the wire; got:\n\t%s", b)
			}
		})
	}
}

// TestValidNextHopIsRendered is the other half: a check for IsValid that
// returned nil unconditionally would pass the test above.
func TestValidNextHopIsRendered(t *testing.T) {
	b := mustMarshal(t, NewWireUnicastRoute(query.Route{NextHop: netip.MustParseAddr("10.0.103.61")}))
	if !bytes.Contains(b, []byte(`"next_hop":"10.0.103.61"`)) {
		t.Errorf("a valid next hop must be rendered; got:\n\t%s", b)
	}
}

// TestPlainAddressFieldsNeverRenderInvalidIP covers the non-nullable address
// fields, which the contract types as plain strings with no null form.
// query never hands one of these an invalid address, so this is about what
// happens if that ever stops being true: "" is unmistakably not an address,
// where "invalid IP" would be rendered and indexed as though it were one.
func TestPlainAddressFieldsNeverRenderInvalidIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"router", NewWireRouter(query.Router{})},
		{"peer", NewWirePeer(query.Peer{})},
		{"unicast", NewWireUnicastRoute(query.Route{})},
		{"vpn", NewWireVPNRoute(query.VPNRoute{})},
		{"evpn", NewWireEVPNRoute(query.EVPNRoute{})},
		{"history", NewWireHistoryEvent(query.HistoryEvent{})},
		{"events", NewWirePeerEvent(query.PeerEvent{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b := mustMarshal(t, tc.v); bytes.Contains(b, []byte("invalid IP")) {
				t.Errorf("netip.Addr{}.String() leaked to the wire; got:\n\t%s", b)
			}
		})
	}
}

// TestU64IdentitiesMarshalAsStrings extends TestSessionIDMarshalsAsString to
// every u64 the contract types as a string, on every type that carries one.
// seq is per-(peer, session) and small today, but it is the same u64 and the
// contract makes the same commitment about it.
func TestU64IdentitiesMarshalAsStrings(t *testing.T) {
	const big = uint64(1774329600123456789)
	b := mustMarshal(t, NewWirePeer(query.Peer{SessionID: big}))
	if !bytes.Contains(b, []byte(`"session_id":"1774329600123456789"`)) {
		t.Errorf("peer session_id must be a quoted string; got:\n\t%s", b)
	}
	b = mustMarshal(t, NewWireHistoryEvent(query.HistoryEvent{SessionID: big, Seq: 18446744073709551615}))
	if !bytes.Contains(b, []byte(`"session_id":"1774329600123456789"`)) {
		t.Errorf("history session_id must be a quoted string; got:\n\t%s", b)
	}
	if !bytes.Contains(b, []byte(`"seq":"18446744073709551615"`)) {
		t.Errorf("seq must be a quoted string, exact at the u64 maximum; got:\n\t%s", b)
	}
	// PeerEvent adds a third u64 no earlier type carries: stream_seq, the
	// tie-breaker peer_events' own physical sort key ends on and this
	// endpoint's cursor position's second half.
	b = mustMarshal(t, NewWirePeerEvent(query.PeerEvent{
		SessionID: big, Seq: 18446744073709551615, StreamSeq: 18446744073709551614,
	}))
	if !bytes.Contains(b, []byte(`"session_id":"1774329600123456789"`)) {
		t.Errorf("peer event session_id must be a quoted string; got:\n\t%s", b)
	}
	if !bytes.Contains(b, []byte(`"seq":"18446744073709551615"`)) {
		t.Errorf("peer event seq must be a quoted string; got:\n\t%s", b)
	}
	if !bytes.Contains(b, []byte(`"stream_seq":"18446744073709551614"`)) {
		t.Errorf("stream_seq must be a quoted string; got:\n\t%s", b)
	}
}

// TestArrayFieldsAreNeverNull walks every array-typed field in the contract.
// All of them are required and typed array with no null form, and a nil Go
// slice marshals to null. query returns non-nil slices from its own scans,
// which is what makes this a guard rather than a fix: it pins the shape
// against a value that did not come from a scan.
func TestArrayFieldsAreNeverNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		want []string
	}{
		{"unicast", NewWireUnicastRoute(query.Route{}),
			[]string{"as_path", "communities", "large_communities", "ext_communities", "route_targets"}},
		{"vpn", NewWireVPNRoute(query.VPNRoute{}),
			[]string{"as_path", "communities", "large_communities", "ext_communities", "route_targets"}},
		{"evpn", NewWireEVPNRoute(query.EVPNRoute{}),
			[]string{"as_path", "communities", "large_communities", "ext_communities", "route_targets", "labels"}},
		{"history", NewWireHistoryEvent(query.HistoryEvent{}), []string{"as_path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keysOf(t, tc.v)
			for _, k := range tc.want {
				if string(got[k]) != "[]" {
					t.Errorf("%s must be [] when empty, never null; got %s", k, got[k])
				}
			}
		})
	}
}

// TestCommunitiesPassThroughQueryRendering pins that this layer renders
// nothing: query.communityStrings already turned the stored Array(UInt32)
// into "65000:100" notation, and a second renderer here would be a second
// place for the shift-and-mask to be wrong.
func TestCommunitiesPassThroughQueryRendering(t *testing.T) {
	r := query.Route{
		Communities:      []string{"65000:100", "65535:65281"},
		LargeCommunities: []string{"65000:1:2"},
	}
	b := mustMarshal(t, NewWireUnicastRoute(r))
	if !bytes.Contains(b, []byte(`"communities":["65000:100","65535:65281"]`)) {
		t.Errorf("communities must pass through as rendered; got:\n\t%s", b)
	}
	if !bytes.Contains(b, []byte(`"large_communities":["65000:1:2"]`)) {
		t.Errorf("large_communities must pass through untouched; got:\n\t%s", b)
	}
}

// TestMEDAndLocalPrefKeepTheirZero is the pointer pair the query layer
// already carries: MED 0 is a real advertised value that wins a tie-break
// against MED 10, so it must not marshal the same as no MED at all.
func TestMEDAndLocalPrefKeepTheirZero(t *testing.T) {
	zero := uint32(0)
	b := mustMarshal(t, NewWireUnicastRoute(query.Route{MED: &zero, LocalPref: &zero}))
	if !bytes.Contains(b, []byte(`"med":0`)) || !bytes.Contains(b, []byte(`"local_pref":0`)) {
		t.Errorf("an advertised 0 must survive; got:\n\t%s", b)
	}
	b = mustMarshal(t, NewWireUnicastRoute(query.Route{}))
	if !bytes.Contains(b, []byte(`"med":null`)) || !bytes.Contains(b, []byte(`"local_pref":null`)) {
		t.Errorf("no MED must be null, not 0; got:\n\t%s", b)
	}
}

// TestOriginASNIsCarriedWhenASPathIsNot is the other half of
// TestOriginASNIsNullWhenASPathIsEmpty: a converter that always returned
// nil would pass that one.
func TestOriginASNIsCarriedWhenASPathIsNot(t *testing.T) {
	b := mustMarshal(t, NewWireUnicastRoute(query.Route{ASPath: []uint32{65001, 65002}, OriginASN: 65002}))
	if !bytes.Contains(b, []byte(`"origin_asn":65002`)) {
		t.Errorf("origin_asn must be carried when as_path is not empty; got:\n\t%s", b)
	}
	// AS 0 at the end of a real path is a real answer, and the one the
	// null-when-empty rule must not swallow.
	b = mustMarshal(t, NewWireUnicastRoute(query.Route{ASPath: []uint32{0}, OriginASN: 0}))
	if !bytes.Contains(b, []byte(`"origin_asn":0`)) {
		t.Errorf("a genuine origin AS 0 must survive; got:\n\t%s", b)
	}
}

// TestTimestampsAreRFC3339UTC pins the date-time format the contract asks
// for, and the Z: clickhouse-go can attach a location to a scanned
// DateTime64, and the wire form must not depend on a server setting.
func TestTimestampsAreRFC3339UTC(t *testing.T) {
	loc := time.FixedZone("UTC+13", 13*60*60)
	ts := time.Date(2026, 8, 24, 15, 4, 5, 0, loc)
	b := mustMarshal(t, NewWireRouter(query.Router{LastSeen: ts}))
	if !bytes.Contains(b, []byte(`"last_seen":"2026-08-24T02:04:05Z"`)) {
		t.Errorf("last_seen must be RFC 3339 in UTC; got:\n\t%s", b)
	}
	b = mustMarshal(t, NewWireHistoryEvent(query.HistoryEvent{TsCollector: ts, TsRouter: time.Unix(0, 0)}))
	if !bytes.Contains(b, []byte(`"ts_collector":"2026-08-24T02:04:05Z"`)) {
		t.Errorf("ts_collector must be RFC 3339 in UTC; got:\n\t%s", b)
	}
	// A router with a dead clock reports 1970, and that is data to report
	// rather than a gap to hide.
	if !bytes.Contains(b, []byte(`"ts_router":"1970-01-01T00:00:00Z"`)) {
		t.Errorf("ts_router must report the router's own clock; got:\n\t%s", b)
	}
}

// TestTopologyValuesLandUnderTheRightKeys is the assertion the handler tests
// structurally cannot make.
//
// api/topology_test.go runs against insertHandlerFixture, which holds no
// withdrawn unicast row under peer A -- every tuple's is_withdraw is 0 -- so
// routes and live_routes are the SAME NUMBER there. NewWireASEdge assigning
// LiveRoutes from e.Routes, or transposing the two, passes every test in that
// file. first_seen was worse: nothing asserted it at all, and a zeroed
// time.Time still marshals as "0001-01-01T00:00:00Z", which satisfies the
// contract's date-time format exactly as well as a real instant does.
//
// live_routes is the field that carries the central rule to the
// client: an edge with live_routes == 0 is one the collector knows about and
// the network no longer uses, which is the whole reason the pair is carried
// instead of a server-computed state string. A transposition here is not a
// cosmetic defect, it is a withdrawn adjacency drawn as a live one.
//
// Every value below is distinct from every other, for
// TestRouteTypesAgreeOnCommonFields' reason: equal fixture values would
// satisfy a converter that read one field twice. The timestamp carries a
// non-UTC location so that stamp's conversion is pinned on these two types
// as well.
func TestTopologyValuesLandUnderTheRightKeys(t *testing.T) {
	ts := time.Date(2026, 8, 24, 15, 4, 5, 0, time.FixedZone("UTC+13", 13*60*60))
	const wantFirstSeen = `"first_seen":"2026-08-24T02:04:05Z"`

	edge := mustMarshal(t, NewWireASEdge(query.ASEdge{
		Src: 1, Dst: 2, Routes: 380, LiveRoutes: 363, FirstSeen: ts,
	}))
	for _, want := range []string{
		`"src":1`, `"dst":2`, `"routes":380`, `"live_routes":363`, wantFirstSeen,
	} {
		if !bytes.Contains(edge, []byte(want)) {
			t.Errorf("WireASEdge does not carry %s; got:\n\t%s", want, edge)
		}
	}

	// Origin and Peer set, Transit clear: roles must carry exactly the two
	// that are true, in the fixed order, so an unconditional append or a
	// dropped flag is visible. The handler test pins the other pairing
	// (transit with observed_peer), which is the combination its fixture can
	// produce.
	node := mustMarshal(t, NewWireASNode(query.ASNode{
		ASN: 65001, Routes: 420, Origin: true, Transit: false, Peer: true,
		FirstSeen: ts,
	}))
	for _, want := range []string{
		`"asn":65001`, `"routes":420`, `"roles":["origin","observed_peer"]`, wantFirstSeen,
	} {
		if !bytes.Contains(node, []byte(want)) {
			t.Errorf("WireASNode does not carry %s; got:\n\t%s", want, node)
		}
	}
}

// TestRouteTypesAgreeOnCommonFields is what lets the three route converters
// spell WireRouteCommon out by hand instead of sharing a fourteen-argument
// constructor whose same-typed parameters a swap would survive. Given the
// same common input, the three must produce byte-identical values for every
// RouteCommon key.
//
// ExtCommunities and RouteTargets carry DIFFERENT values below, on purpose:
// RouteTargets moved into WireRouteCommon and ExtCommunities was added
// beside it, so all three converters now assign both by
// hand, and a crossed assignment (ExtCommunities set from r.RouteTargets, or
// the reverse) would still produce a value of the right type and shape --
// only different content catches it.
func TestRouteTypesAgreeOnCommonFields(t *testing.T) {
	med, lp := uint32(0), uint32(100)
	nh := netip.MustParseAddr("10.0.103.61")
	routerIP, peerIP := netip.MustParseAddr("10.0.103.1"), netip.MustParseAddr("10.0.103.2")
	extComm := []string{"rt:65000:1"}
	rt := []string{"65000:1"}
	uni := NewWireUnicastRoute(query.Route{
		RouterSysName: "r1", RouterIP: routerIP, PeerIP: peerIP, NextHop: nh,
		Collector: "c1", RIB: "in_post", PathID: 7,
		ASPath: []uint32{65001, 65002}, OriginASN: 65002, MED: &med, LocalPref: &lp,
		Communities: []string{"65000:100"}, LargeCommunities: []string{"65000:1:2"},
		ExtCommunities: extComm, RouteTargets: rt,
		DumpState: "complete",
	})
	vpn := NewWireVPNRoute(query.VPNRoute{
		RouterSysName: "r1", RouterIP: routerIP, PeerIP: peerIP, NextHop: nh,
		Collector: "c1", RIB: "in_post", PathID: 7,
		ASPath: []uint32{65001, 65002}, OriginASN: 65002, MED: &med, LocalPref: &lp,
		Communities: []string{"65000:100"}, LargeCommunities: []string{"65000:1:2"},
		ExtCommunities: extComm, RouteTargets: rt,
		DumpState: "complete",
	})
	evpn := NewWireEVPNRoute(query.EVPNRoute{
		RouterSysName: "r1", RouterIP: routerIP, PeerIP: peerIP, NextHop: nh,
		Collector: "c1", RIB: "in_post", PathID: 7,
		ASPath: []uint32{65001, 65002}, OriginASN: 65002, MED: &med, LocalPref: &lp,
		Communities: []string{"65000:100"}, LargeCommunities: []string{"65000:1:2"},
		ExtCommunities: extComm, RouteTargets: rt,
		DumpState: "complete",
	})
	u, v, e := keysOf(t, uni), keysOf(t, vpn), keysOf(t, evpn)
	for _, k := range []string{
		"router_sysname", "router_ip", "peer_ip", "collector", "rib", "path_id",
		"next_hop", "as_path", "origin_asn", "med", "local_pref", "communities",
		"large_communities", "ext_communities", "route_targets", "dump_state",
	} {
		if len(u[k]) == 0 {
			t.Fatalf("unicast is missing the common key %q", k)
		}
		if string(u[k]) != string(v[k]) {
			t.Errorf("%s: unicast %s, vpn %s", k, u[k], v[k])
		}
		if string(u[k]) != string(e[k]) {
			t.Errorf("%s: unicast %s, evpn %s", k, u[k], e[k])
		}
	}
}

// contract is the parts of api/openapi.yaml the two tests below read. The
// contract was written before any of this code and is the authority on
// field names and required lists, so these tests ask it rather than
// restating it -- a hardcoded copy of a required list could drift from the
// file it claims to mirror without either one looking wrong.
type contract struct {
	Components struct {
		Schemas map[string]schemaNode `yaml:"schemas"`
	} `yaml:"components"`
}

type schemaNode struct {
	Ref        string               `yaml:"$ref"`
	Required   []string             `yaml:"required"`
	Properties map[string]yaml.Node `yaml:"properties"`
	AllOf      []schemaNode         `yaml:"allOf"`
}

func loadContract(t *testing.T) *contract {
	t.Helper()
	b, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var c contract
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse contract: %v", err)
	}
	if len(c.Components.Schemas) == 0 {
		t.Fatal("contract carries no schemas -- the parse is wrong, not the file")
	}
	return &c
}

// resolve flattens one schema's allOf chain into the required list and the
// property set it commits to.
func (c *contract) resolve(t *testing.T, name string) (required []string, props map[string]bool) {
	t.Helper()
	n, ok := c.Components.Schemas[name]
	if !ok {
		t.Fatalf("contract has no schema %q", name)
	}
	props = map[string]bool{}
	var walk func(schemaNode)
	walk = func(n schemaNode) {
		if n.Ref != "" {
			r, p := c.resolve(t, strings.TrimPrefix(n.Ref, "#/components/schemas/"))
			required = append(required, r...)
			for k := range p {
				props[k] = true
			}
			return
		}
		required = append(required, n.Required...)
		for k := range n.Properties {
			props[k] = true
		}
		for _, a := range n.AllOf {
			walk(a)
		}
	}
	walk(n)
	return required, props
}

// wireValues pairs each contract schema with a zero-value instance of the
// wire type that answers for it. Zero values are the point: a required key
// that is only present when its value is interesting is exactly the defect
// these two tests exist to catch, and an omitempty tag is invisible until
// something is empty.
func wireValues() map[string]any {
	return map[string]any{
		"Router":       NewWireRouter(query.Router{}),
		"Peer":         NewWirePeer(query.Peer{}),
		"UnicastRoute": NewWireUnicastRoute(query.Route{}),
		"VPNRoute":     NewWireVPNRoute(query.VPNRoute{}),
		"EVPNRoute":    NewWireEVPNRoute(query.EVPNRoute{}),
		"HistoryEvent": NewWireHistoryEvent(query.HistoryEvent{}),
		"PeerEvent":    NewWirePeerEvent(query.PeerEvent{}),
		"RouteFanout":  NewWireRouteFanout(nil, nil, nil),
		// The three topology schemas are entered separately rather than only
		// through TopologyFanout: keysOf reads TOP-LEVEL keys, so the fanout
		// entry alone would check three key names and nothing inside them.
		"TopologyFanout": NewWireTopologyFanout(query.Graph{}, query.Graph{}, query.Graph{}),
		"Graph":          NewWireGraph(query.Graph{}),
		"ASNode":         NewWireASNode(query.ASNode{}),
		"ASEdge":         NewWireASEdge(query.ASEdge{}),
		"Meta":           Meta{},
		"ErrorResponse":  ErrorResponse{},
	}
}

// TestRequiredKeysArePresentOnAZeroValue reads the contract's own required
// lists. required there means the key is always present, NOT that its value
// is non-null: a caller must never have to tell "the server omitted this"
// from "this route has no MED", so the absent case simply does not occur.
// omitempty on any field below breaks that, and only on the empty values a
// hand-written test is least likely to construct.
func TestRequiredKeysArePresentOnAZeroValue(t *testing.T) {
	c := loadContract(t)
	for name, v := range wireValues() {
		t.Run(name, func(t *testing.T) {
			required, _ := c.resolve(t, name)
			if len(required) == 0 {
				t.Fatalf("%s has no required keys in the contract -- the resolve is wrong", name)
			}
			got := keysOf(t, v)
			for _, k := range required {
				if _, ok := got[k]; !ok {
					t.Errorf("%s: required key %q is missing from the wire form:\n\t%s",
						name, k, mustMarshal(t, v))
				}
			}
		})
	}
}

// TestEmittedKeysAreDocumented is the same comparison from the other side:
// a key this package emits that the contract does not describe is either a
// misspelled json tag or an undocumented field, and both are contract
// violations. It is what makes the test above more than a spell-checker --
// a typo'd tag fails both, and only one of them names it.
func TestEmittedKeysAreDocumented(t *testing.T) {
	c := loadContract(t)
	for name, v := range wireValues() {
		t.Run(name, func(t *testing.T) {
			_, props := c.resolve(t, name)
			for k := range keysOf(t, v) {
				if !props[k] {
					t.Errorf("%s: emitted key %q is not in the contract's properties", name, k)
				}
			}
		})
	}
}

// TestMetaOptionalKeysAreDocumented is the test above with the one blind
// spot it structurally cannot cover: wireValues hands it a ZERO value of
// every wire type, on purpose (see that function's own doc comment), and
// every optional Meta field carries omitempty -- so Meta{} emits three keys
// and the other eight are never seen by any cross-check in this package.
//
// That is not a theoretical gap. Meta.activity_window reached the wire, the
// contract, the UI and a passing test suite with its NAME cross-checked
// nowhere: TestCollectorsNamesItsActivityWindowInMeta decodes through the Go
// type, so a tag typo round-trips cleanly; api/openapi_test.go's populated
// Meta golden validates against a schema that sets no additionalProperties:
// false and does not require the key, so an extra key validates. Verified by
// mutation: renaming the tag to "activity_windwo" left the WHOLE api package
// green, and fails here.
//
// A populated Meta rather than a second entry in wireValues, because that
// map is shared with TestRequiredKeysArePresentOnAZeroValue, whose whole
// claim is about zero values.
func TestMetaOptionalKeysAreDocumented(t *testing.T) {
	// Every optional field set to something non-empty: omitempty makes a
	// field invisible to this check exactly when it is empty, so a field
	// left at its zero value here is a field this test does not cover.
	full := Meta{
		NextCursor:       new("opaque"),
		Warnings:         Warnings{{Code: WarnSessionDumping, Message: "peer 10.0.103.61 is mid-dump"}},
		TotalMatched:     new(uint64(3250)),
		CommunityColumns: []string{"live_communities"},
		DumpTotals:       &WireDumpTotals{Archived: 100, Dumps: 40, Changes: 60},
		SessionTotals:    &WireSessionTotals{Sessions: 9, Up: 4, Down: 3, ViewLost: 2},
		LocRIBTotals:     &WireLocRIBTotals{Reported: 140, Archived: 610},
		FlagTotals:       &WireFlagTotals{Envelopes: 58},
		ChurnBucket:      new("5m0s"),
		ChurnFrom:        new(goldenTime()),
		ChurnTo:          new(goldenTime()),
		ActivityWindow:   new("30m0s"),
		RetentionDays:    new(90),
		ASNamesLoaded:    new(true),
		ASNamesPublished: new(goldenTime()),
	}

	c := loadContract(t)
	_, props := c.resolve(t, "Meta")
	got := keysOf(t, full)
	for k := range got {
		if !props[k] {
			t.Errorf("Meta: emitted key %q is not in the contract's properties", k)
		}
	}
	// The count is the guard on the FIXTURE, not on Meta: every key above
	// is hand-written, so a property the contract documents and this
	// literal does not emit is a Meta field going uncovered here -- which
	// is the exact shape of the gap this test was added to close, one field
	// later. Checked by mutation: adding a property to the contract's Meta
	// without adding it here fails.
	if len(got) != len(props) {
		t.Errorf("this fixture emits %d keys and the contract documents %d Meta properties; "+
			"a field added to Meta must be added here too or it goes uncovered\n\tgot: %s",
			len(got), len(props), mustMarshal(t, full))
	}
}

// TestEnvelopeShape pins the two keys every 200 response carries, and that
// next_cursor is present-and-null rather than absent on a last page.
func TestEnvelopeShape(t *testing.T) {
	b := mustMarshal(t, Envelope{Data: []WireRouter{}, Meta: Meta{}})
	if string(b) != `{"data":[],"meta":{"next_cursor":null,"warnings":[],"total_matched":null}}` {
		t.Errorf("envelope shape changed; got:\n\t%s", b)
	}
	cur := "opaque"
	b = mustMarshal(t, Envelope{Data: []int{}, Meta: Meta{NextCursor: &cur}})
	if !bytes.Contains(b, []byte(`"next_cursor":"opaque"`)) {
		t.Errorf("next_cursor must be carried when there is another page; got:\n\t%s", b)
	}
}

// TestErrorResponseShape pins the nesting: the contract puts code and
// message inside an "error" object, not at the top level.
func TestErrorResponseShape(t *testing.T) {
	b := mustMarshal(t, ErrorResponse{Error: ErrorBody{Code: ErrInvalidParam, Message: "prefix and covers are mutually exclusive"}})
	want := `{"error":{"code":"invalid_param","message":"prefix and covers are mutually exclusive"}}`
	if string(b) != want {
		t.Errorf("error shape changed;\n\tgot  %s\n\twant %s", b, want)
	}
}

// TestGoldenVPNRoute is one fully populated route, pinned byte for byte:
// the tests above each check one property, and this checks that they hold
// together in one object -- key order, the u64 string, the label that is a
// real 0, and the RD that is genuinely "" for lu4 rather than a rendered
// placeholder.
func TestGoldenVPNRoute(t *testing.T) {
	med, lp := uint32(0), uint32(100)
	got := mustMarshal(t, NewWireVPNRoute(query.VPNRoute{
		RouterSysName:    "ceos-1",
		RouterIP:         netip.MustParseAddr("10.0.103.61"),
		PeerIP:           netip.MustParseAddr("10.0.103.62"),
		NextHop:          netip.MustParseAddr("10.0.103.62"),
		Collector:        "cml",
		RIB:              "in_post",
		Family:           "vpn4",
		RD:               "65000:1",
		Prefix:           "10.77.0.0/24",
		PathID:           1,
		Label:            0,
		HasLabel:         true,
		RouteTargets:     []string{"65000:1"},
		ASPath:           []uint32{65001, 65002},
		OriginASN:        65002,
		MED:              &med,
		LocalPref:        &lp,
		Communities:      []string{"65000:100"},
		LargeCommunities: []string{"65000:1:2"},
		ExtCommunities:   []string{"rt:65000:1"},
		DumpState:        "complete",
	}))
	want := `{"router_sysname":"ceos-1","router_ip":"10.0.103.61","peer_ip":"10.0.103.62",` +
		`"collector":"cml","rib":"in_post","path_id":1,"next_hop":"10.0.103.62",` +
		`"as_path":[65001,65002],"origin_asn":65002,"med":0,"local_pref":100,` +
		`"communities":["65000:100"],"large_communities":["65000:1:2"],` +
		`"ext_communities":["rt:65000:1"],"route_targets":["65000:1"],"dump_state":"complete",` +
		`"family":"vpn4","rd":"65000:1","prefix":"10.77.0.0/24","label":0}`
	if string(got) != want {
		t.Errorf("golden VPN route changed;\n\tgot  %s\n\twant %s", got, want)
	}
}

// TestLSU64sMarshalAsStrings extends TestU64IdentitiesMarshalAsStrings to
// the two u64s link-state adds. node_key is a cityHash64 and routinely
// exceeds 2^53, so a JSON number would be silently rounded by a JavaScript
// client or an LLM's parser -- and two distinct nodes would collide into
// one key.
func TestLSU64sMarshalAsStrings(t *testing.T) {
	const big = uint64(17976931348623157)
	b := mustMarshal(t, NewWireLSNode(query.LSNode{NodeKey: big, Identifier: big}))
	for _, want := range []string{
		`"node_key":"17976931348623157"`,
		`"identifier":"17976931348623157"`,
	} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("want %s on the wire; got:\n\t%s", want, b)
		}
	}
}

// TestLSArraysAreNeverNull. sr_algorithms and adj_sids are arrays the
// contract types as arrays; a nil slice marshals to null, which reads as
// "unknown" to a client checking for the key rather than as "none".
func TestLSArraysAreNeverNull(t *testing.T) {
	node := mustMarshal(t, NewWireLSNode(query.LSNode{}))
	if !bytes.Contains(node, []byte(`"sr_algorithms":[]`)) {
		t.Errorf("sr_algorithms must marshal as []; got:\n\t%s", node)
	}
	link := mustMarshal(t, NewWireLSLink(query.LSLink{}))
	if !bytes.Contains(link, []byte(`"adj_sids":[]`)) {
		t.Errorf("adj_sids must marshal as []; got:\n\t%s", link)
	}
}

// TestProtocolNameTravelsWithItsNumber pins both halves of the contract:
// a known ID carries its name, an unknown one carries ""
// rather than a guess, and the number is present either way. Nothing else
// in this repo maps Protocol-IDs to names -- the dashboards render a bare
// integer -- so an API for humans and LLM tooling is the first consumer
// that needs one.
func TestProtocolNameTravelsWithItsNumber(t *testing.T) {
	known := mustMarshal(t, NewWireLSNode(query.LSNode{Protocol: 2}))
	if !bytes.Contains(known, []byte(`"protocol":2`)) ||
		!bytes.Contains(known, []byte(`"protocol_name":"isis-l2"`)) {
		t.Errorf("a known protocol must carry both forms; got:\n\t%s", known)
	}
	unknown := mustMarshal(t, NewWireLSNode(query.LSNode{Protocol: 200}))
	if !bytes.Contains(unknown, []byte(`"protocol":200`)) ||
		!bytes.Contains(unknown, []byte(`"protocol_name":""`)) {
		t.Errorf("an unknown protocol must carry its number and an empty name; "+
			"got:\n\t%s", unknown)
	}
}

// TestLSEndpointLabelSurvivesTheZeroValue. The label chain ends in the raw
// router_id so it is never empty -- but query/ builds the chain, not this
// package, so what this asserts is only that the wire form does not DROP a
// label that was there. Both ends are checked: one converter written and one
// copy-pasted is exactly how the remote end ends up carrying the local
// end's data.
func TestLSEndpointLabelSurvivesTheZeroValue(t *testing.T) {
	b := mustMarshal(t, NewWireLSLink(query.LSLink{
		Local:  query.LSEndpoint{RouterID: "0a0000f1", Label: "near", LabelSource: "observer"},
		Remote: query.LSEndpoint{RouterID: "0a0000f2", Label: "far", LabelSource: "fleet"},
	}))
	for _, want := range []string{
		`"label":"near"`, `"label_source":"observer"`,
		`"label":"far"`, `"label_source":"fleet"`,
	} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("want %s; got:\n\t%s", want, b)
		}
	}
}

// TestWirePeerEventKeepsUnspecifiedAsItself. peer_events.kind is
// Enum8('unspecified' = 0, ...) and sink/rows.go writes 'unspecified' for
// any PeerEvent the BMP stream did not settle -- so it is a real stored
// value, and openapi.yaml names it in kind's own enum. The wire layer
// copies it through with no branch today, which is exactly why this is
// worth a test: the realistic regression is not someone rewriting that
// assignment, it is someone adding a "normalize the kind" helper and
// folding the unsettled case into a settled one. That is the same defect
// this project already paid for once with dump_state's 'unknown', and it
// would leave the screen reporting a session came up on evidence nobody
// has.
func TestWirePeerEventKeepsUnspecifiedAsItself(t *testing.T) {
	b := mustMarshal(t, NewWirePeerEvent(query.PeerEvent{Kind: "unspecified"}))
	if !bytes.Contains(b, []byte(`"kind":"unspecified"`)) {
		t.Errorf("the wire layer must carry an unsettled kind as itself, "+
			"never collapsed onto up/down/view_lost; got:\n\t%s", b)
	}
}
