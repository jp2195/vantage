package schema_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	in := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: "10.0.0.1", SysName: "rr1"},
		Peer:        &vantagev1.PeerId{Ip: "10.0.0.9", Asn: 65001},
		SessionId:   123, Seq: 1,
		ParseFlags: []vantagev1.ParseFlag{vantagev1.ParseFlag_PARSE_FLAG_TS_COLLECTOR_FALLBACK},
		Payload: &vantagev1.Envelope_Route{Route: &vantagev1.RouteEvent{
			Family:    &vantagev1.Family{Afi: 1, Safi: 1},
			Announced: []*vantagev1.Prefix{{Prefix: "192.0.2.0/24"}},
		}},
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := &vantagev1.Envelope{}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("round trip mismatch:\nin:  %v\nout: %v", in, out)
	}
}

func TestVpnAndEvpnRoundTrip(t *testing.T) {
	env := &vantagev1.Envelope{
		CollectorId: "c1",
		Payload: &vantagev1.Envelope_Route{Route: &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 1, Safi: 128},
			VpnAnnounced: []*vantagev1.VpnPrefix{{
				Prefix: "10.0.0.0/24", PathId: 7, Rd: "65000:100", Labels: []uint32{24001},
			}},
			EvpnAnnounced: []*vantagev1.EvpnRoute{{
				RouteType: 2, Rd: "65000:1", Mac: "aa:bb:cc:dd:ee:ff", Ip: "10.1.1.1",
				EthernetTag: 0, Esi: "00000000000000000000", Labels: []uint32{10100},
			}},
		}},
	}
	b, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	got := &vantagev1.Envelope{}
	if err := proto.Unmarshal(b, got); err != nil {
		t.Fatal(err)
	}
	// proto.Equal on the whole envelope rather than spot-checking a few
	// fields: the earlier version populated eleven and asserted four, so a
	// field that failed to survive marshaling would have gone unnoticed.
	//
	// Be clear about what this does NOT prove. A round-trip cannot detect a
	// wrong field *number* -- marshal and unmarshal share the same generated
	// code, so any self-consistent numbering round-trips perfectly. Verified
	// by mutation: renumbering a field and regenerating leaves this test
	// green. Field numbers are protected by `buf breaking` in CI (which
	// catches renumbering an existing field, verified to have teeth) and by
	// reading the descriptor at review time. For a brand-new field there is
	// no automated check at all, because there is no prior contract to
	// violate until it ships.
	if !proto.Equal(env, got) {
		t.Fatalf("round-trip differs:\n sent %+v\n got  %+v", env, got)
	}
	r := got.GetRoute()
	if len(r.VpnAnnounced) != 1 || len(r.EvpnAnnounced) != 1 {
		t.Fatalf("expected one prefix of each kind: %+v", r)
	}
	// Pinned explicitly because it is the field most likely to be "corrected"
	// by someone who reads it as an unshifted MPLS label. It is a VNI.
	if r.EvpnAnnounced[0].Labels[0] != 10100 {
		t.Fatalf("EvpnRoute.Labels[0] = %d, want the raw 24-bit value 10100",
			r.EvpnAnnounced[0].Labels[0])
	}
	if vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED != 8 {
		t.Fatalf("PARSE_FLAG_NLRI_UNTYPED = %d, want 8", vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED)
	}
	raw := &vantagev1.RawEvent{Mirrored: true}
	if !raw.Mirrored {
		t.Fatal("RawEvent.Mirrored not settable")
	}
}

func TestLsNodeLinkRoundTrip(t *testing.T) {
	in := &vantagev1.Envelope{
		CollectorId: "c1",
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{{
				Protocol: 3, Identifier: 100, Name: "xr-rr1",
				SrgbBase: 16000, SrgbSize: 8000,
				Local: &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 1}},
			}},
			Links: []*vantagev1.LsLink{{
				Protocol: 3, Identifier: 100, TeMetric: 1,
				AdjSids: []*vantagev1.LsAdjacencySid{{Sid: 24001}},
				Local:   &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 2}},
				Remote:  &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 5}},
			}},
			UnknownTlvs: map[uint32][]byte{0xFFFF: {0xBE, 0xEF}},
		}},
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out vantagev1.Envelope
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	ls := out.GetLs()
	if len(ls.GetNodes()) != 1 || ls.GetNodes()[0].GetSrgbBase() != 16000 {
		t.Fatalf("node did not round-trip: %v", ls.GetNodes())
	}
	if len(ls.GetLinks()) != 1 || len(ls.GetLinks()[0].GetAdjSids()) != 1 || ls.GetLinks()[0].GetAdjSids()[0].GetSid() != 24001 {
		t.Fatalf("link did not round-trip: %v", ls.GetLinks())
	}
	if string(ls.GetUnknownTlvs()[0xFFFF]) != "\xbe\xef" {
		t.Fatalf("unknown TLVs did not round-trip: %v", ls.GetUnknownTlvs())
	}
}

// TestCollectorBeatRoundTrip pins that started_at keeps its nanoseconds. The
// read side compares it to session_id, which is a UnixNano value on the same
// clock: a started_at truncated to the microsecond would sit up to 999 ns
// below the first session of its own process, and that session would read as
// belonging to an earlier one.
func TestCollectorBeatRoundTrip(t *testing.T) {
	started := time.Date(2026, 9, 23, 10, 0, 0, 123456789, time.UTC)
	in := &vantagev1.Envelope{
		CollectorId: "dev-c1",
		TsCollector: timestamppb.New(started.Add(42 * time.Second)),
		Payload: &vantagev1.Envelope_Beat{Beat: &vantagev1.CollectorBeat{
			StartedAt: timestamppb.New(started),
		}},
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := &vantagev1.Envelope{}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("round trip mismatch:\nin:  %v\nout: %v", in, out)
	}
	if got := out.GetBeat().GetStartedAt().AsTime(); !got.Equal(started) {
		t.Fatalf("started_at = %v, want %v (nanoseconds must survive)", got, started)
	}
}
