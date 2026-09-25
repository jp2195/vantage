package sink

import (
	"encoding/hex"
	"fmt"
	"maps"
	"net/netip"

	"github.com/jp2195/vantage/bgp"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// RowsFor translates one envelope into its ClickHouse rows. Pure: it reads
// only its arguments and allocates only what it returns.
//
// streamSeq is the JetStream stream sequence of the message that carried this
// envelope. It is part of every table's sort key and is what makes
// at-least-once redelivery collapse at merge time rather than duplicating.
//
// It returns an error, and no rows, for an envelope carrying an address
// ClickHouse cannot store; see checkAddresses.
func RowsFor(env *vantagev1.Envelope, streamSeq uint64) (Rows, error) {
	if env == nil {
		return Rows{}, nil
	}
	if err := checkAddresses(env); err != nil {
		return Rows{}, err
	}
	e := commonColumns(env, streamSeq)
	var out Rows
	switch p := env.GetPayload().(type) {
	case *vantagev1.Envelope_Route:
		out = routeRows(e, p.Route)
	case *vantagev1.Envelope_Ls:
		unknown := map[uint16]string{}
		for k, v := range p.Ls.GetUnknownTlvs() {
			unknown[uint16(k)] = string(v)
		}
		for _, n := range p.Ls.GetNodes() {
			d := n.GetLocal()
			out.LsNodes = append(out.LsNodes, LsNodeRow{
				envelope: e, Protocol: uint8(n.GetProtocol()), Identifier: n.GetIdentifier(),
				ASN: d.GetAsn(), BgplsID: d.GetBgplsId(), Area: d.GetArea(),
				RouterID: hex.EncodeToString(d.GetRouterId()),
				// RouterIDv4 was decoded (attribute TLV 1028) and put on the wire
				// (LsNode.router_id_v4), but nothing between here and ClickHouse
				// ever read it -- ls_nodes.router_id stayed hex-only, defeating
				// the whole point of a topology visualization by rendering every
				// node label as hex. RouterID itself stays hex/raw: IS-IS system
				// IDs are 6 bytes and not addresses, so it cannot be replaced,
				// only supplemented.
				RouterIDv4: ipString(n.GetRouterIdV4()),
				IsWithdraw: boolToUint8(n.GetIsWithdraw()), Name: n.GetName(),
				SrgbBase: n.GetSrgbBase(), SrgbSize: n.GetSrgbSize(),
				SrlbBase: n.GetSrlbBase(), SrlbSize: n.GetSrlbSize(),
				SrAlgorithms: uint32sToUint8s(n.GetSrAlgorithms()),
				// mergedUnknownTLVs folds this node's own
				// NLRI-level unknown TLVs (n.GetUnknownTlvs()) in with the
				// shared attribute-level ones (unknown) -- see its doc
				// comment for why the DDL only has room for the merge.
				UnknownTLVs: mergedUnknownTLVs(unknown, n.GetUnknownTlvs()),
			})
		}
		for _, l := range p.Ls.GetLinks() {
			lo, re := l.GetLocal(), l.GetRemote()
			out.LsLinks = append(out.LsLinks, LsLinkRow{
				envelope: e, Protocol: uint8(l.GetProtocol()), Identifier: l.GetIdentifier(),
				LocalASN: lo.GetAsn(), LocalBgplsID: lo.GetBgplsId(), LocalArea: lo.GetArea(),
				LocalRouterID: hex.EncodeToString(lo.GetRouterId()),
				RemoteASN:     re.GetAsn(), RemoteBgplsID: re.GetBgplsId(), RemoteArea: re.GetArea(),
				RemoteRouterID: hex.EncodeToString(re.GetRouterId()),
				LocalIfAddr:    ipString(l.GetLocalIfaddr()), RemoteIfAddr: ipString(l.GetRemoteIfaddr()),
				LinkLocalID: l.GetLinkLocalId(), LinkRemoteID: l.GetLinkRemoteId(),
				IsWithdraw:    boolToUint8(l.GetIsWithdraw()),
				AdjSIDs:       adjSidValues(l.GetAdjSids()),
				AdjSIDFlags:   adjSidFlags(l.GetAdjSids()),
				AdjSIDWeights: adjSidWeights(l.GetAdjSids()),
				TEMetric:      l.GetTeMetric(), IGPMetric: l.GetIgpMetric(),
				AdminGroup: l.GetAdminGroup(), MaxBandwidth: l.GetMaxBandwidth(),
				UnknownTLVs: mergedUnknownTLVs(unknown, l.GetUnknownTlvs()),
			})
		}
		for _, pfx := range p.Ls.GetPrefixes() {
			d := pfx.GetLocal()
			out.LsPrefixes = append(out.LsPrefixes, LsPrefixRow{
				envelope: e, Protocol: uint8(pfx.GetProtocol()), Identifier: pfx.GetIdentifier(),
				ASN: d.GetAsn(), BgplsID: d.GetBgplsId(), Area: d.GetArea(),
				// Hex, matching LsNodeRow.RouterID: node_key is derived in
				// ClickHouse from these six columns, so a prefix only joins to
				// its node if the router-id is stored the same way on both.
				RouterID: hex.EncodeToString(d.GetRouterId()),
				// ipString, not hex: a prefix is always an address, and a
				// looking-glass query over hex is unusable.
				Prefix: ipString(pfx.GetPrefix()), PrefixLen: uint8(pfx.GetPrefixLen()),
				IsWithdraw:      boolToUint8(pfx.GetIsWithdraw()),
				PrefixSID:       pfx.GetPrefixSid(),
				PrefixSIDFlags:  uint8(pfx.GetPrefixSidFlags()),
				HasPrefixSID:    boolToUint8(pfx.GetHasPrefixSid()),
				PrefixMetric:    pfx.GetPrefixMetric(),
				PrefixAttrFlags: uint8(pfx.GetPrefixAttrFlags()),
				OSPFRouteType:   uint8(pfx.GetOspfRouteType()),
				UnknownTLVs:     mergedUnknownTLVs(unknown, pfx.GetUnknownTlvs()),
			})
		}
		// Emit a raw ls_events row whenever there is content that belongs
		// there -- RawReach/RawUnreach non-empty, OR an LS End-of-RIB, which
		// carries neither NLRI nor raw bytes at all but must still be
		// recorded: it is how a topology consumer tells a converged LS RIB
		// from one still loading, the same way an eor_events row does for
		// the BGP families.
		//
		// bgp/update.go's parseMPReach/parseMPUnreach set RawReach/RawUnreach
		// non-empty on a MIXED decode too: one MP_REACH can carry Node/Link
		// NLRI this build types plus a Prefix NLRI (BGP-LS types 3/4) it does
		// not, and per that code's own comment "the raw ones are only
		// recoverable from here". Gating this on len(out.LsNodes) == 0 &&
		// len(out.LsLinks) == 0 would skip the raw row whenever ANY node/link
		// decoded, silently dropping the undecoded remainder into no table at
		// all -- a data-loss defect.
		//
		// So one envelope can legitimately produce BOTH typed rows AND a raw
		// row. That is correct, not double counting for the typed tables --
		// but the raw row itself is NOT confined to only the undecoded
		// remainder: parseMPReach/parseMPUnreach store
		// RawReach/RawUnreach as the ENTIRE MP_REACH/MP_UNREACH attribute
		// value -- AFI/SAFI/next-hop/reserved/NLRI -- whenever ANY part went
		// untyped, which includes the wire bytes for whatever Node/Link DID
		// decode into a typed row above, not only the part that didn't. A
		// reprocessor reading ls_events.raw_reach/raw_unreach to recover NLRI
		// this build could not decode must therefore expect to also see NLRI
		// it already has a typed row for elsewhere, and cannot treat every
		// byte in that column as otherwise-lost data. Do not "fix" the
		// both-typed-and-raw behavior itself back to an either/or.
		if len(p.Ls.GetRawReach()) > 0 || len(p.Ls.GetRawUnreach()) > 0 || p.Ls.GetEndOfRib() {
			out.Ls = []LsRow{{
				envelope: e, Family: familyName(p.Ls.GetFamily()),
				RawReach: p.Ls.GetRawReach(), RawUnreach: p.Ls.GetRawUnreach(),
				EndOfRIB: boolToUint8(p.Ls.GetEndOfRib()),
			}}
		}
	case *vantagev1.Envelope_PeerEvent:
		out.Peer = []PeerRow{peerRow(e, env.GetRouterInfo().GetSysDescr(), p.PeerEvent)}
	case *vantagev1.Envelope_Stats:
		out.Stats = []StatsRow{{envelope: e, Counters: p.Stats.GetCounters()}}
	case *vantagev1.Envelope_Beat:
		out.Beats = []BeatRow{{
			CollectorID: env.GetCollectorId(),
			StartedAt:   p.Beat.GetStartedAt().AsTime(),
			BeatAt:      env.GetTsCollector().AsTime(),
		}}
	}
	return out, nil
}

// checkAddresses refuses an envelope whose address strings would fail at
// insert time: every string that feeds an IPv4/IPv6 column (see
// clickHouseIPv6's doc comment for the list) must be empty or parse, and
// peer_bgp_id, the one IPv4 column, must be an IPv4 address.
//
// It exists because an insert failure is not per envelope. The consumer
// leaves the whole batch unacked for redelivery, so one envelope ClickHouse
// refuses fails every batch it lands in, and the writer stops archiving
// everything behind it for good. The collector only ever publishes
// addresses it parsed, so this is a guard against a bad or hostile
// publisher on the bus rather than a case the pipeline produces -- and
// the IPv4 check is sharper than an insert error: the driver converts with
// netip.Addr.As4, which panics on an IPv6 address.
//
// Addresses carried as bytes (ipString's callers) need no check: they are
// rendered from a netip.Addr and are valid by construction.
func checkAddresses(env *vantagev1.Envelope) error {
	for _, f := range []struct {
		name, value string
		v4          bool
	}{
		{"router.ip", env.GetRouter().GetIp(), false},
		{"peer.ip", env.GetPeer().GetIp(), false},
		{"peer.bgp_id", env.GetPeer().GetBgpId(), true},
		{"peer_event.local_ip", env.GetPeerEvent().GetLocalIp(), false},
	} {
		if f.value == "" {
			continue
		}
		a, err := netip.ParseAddr(f.value)
		if err != nil {
			return fmt.Errorf("%s %q is not an IP address", f.name, f.value)
		}
		if f.v4 && !a.Unmap().Is4() {
			return fmt.Errorf("%s %q is not an IPv4 address", f.name, f.value)
		}
	}
	return nil
}

func commonColumns(env *vantagev1.Envelope, streamSeq uint64) envelope {
	flags := make([]string, 0, len(env.GetParseFlags()))
	for _, f := range env.GetParseFlags() {
		flags = append(flags, f.String())
	}
	return envelope{
		CollectorID:   env.GetCollectorId(),
		RouterIP:      clickHouseIPv6(env.GetRouter().GetIp()),
		RouterSysname: env.GetRouter().GetSysName(),
		PeerIP:        clickHouseIPv6(env.GetPeer().GetIp()),
		RIB:           ribName(env.GetPeer()),
		PeerASN:       env.GetPeer().GetAsn(),
		PeerBGPID:     clickHouseIPv4(env.GetPeer().GetBgpId()),
		SessionID:     env.GetSessionId(),
		Seq:           env.GetSeq(),
		TsRouter:      env.GetTsRouter().AsTime(),
		TsCollector:   env.GetTsCollector().AsTime(),
		ParseFlags:    flags,
		StreamSeq:     streamSeq,
	}
}

// ribName renders PeerId's O flag (RFC 8671) crossed with its L flag as the
// rib column's enum text. The collector already publishes the four streams on
// four subjects with four sequence counters; this keeps them four rows.
//
// The Loc-RIB peer type (RFC 9069) is answered before either flag is read.
// A Loc-RIB dump is neither adj-RIB-in nor adj-RIB-out -- it is the router's
// own table after best-path selection -- so it gets its own enum value rather
// than defaulting into in_pre, which would make "how many routes did we
// receive" silently include routes nobody sent us. RFC 9069 §4.2 also
// redefines the flags byte for this peer type, leaving L and O reserved, so
// acting on them here would scatter one Loc-RIB peer across four rib values
// on whatever junk a sender left in those bits. subjects.ribDirection and
// collector.adjRIBOut exclude the peer type for the same reason; all three
// must agree, since they key the subject, the parse capabilities and the
// column.
func ribName(p *vantagev1.PeerId) string {
	if p.GetType() == vantagev1.PeerType_PEER_TYPE_LOC_RIB {
		return "loc_rib"
	}
	switch {
	case p.GetAdjRibOut() && p.GetPostPolicy():
		return "out_post"
	case p.GetAdjRibOut():
		return "out_pre"
	case p.GetPostPolicy():
		return "in_post"
	default:
		return "in_pre"
	}
}

// familyNames is the family-token registry (see
// subjects.FamilyToken), transcribed rather than imported: rows.go
// stays pure (schema + bgp only), and this is the same registry so a
// family string means the same thing in a subject and in a column.
var familyNames = map[bgp.Family]string{
	{AFI: 1, SAFI: 1}:      "ipv4u",
	{AFI: 2, SAFI: 1}:      "ipv6u",
	{AFI: 1, SAFI: 4}:      "lu4",
	{AFI: 1, SAFI: 128}:    "vpn4",
	{AFI: 2, SAFI: 128}:    "vpn6",
	{AFI: 25, SAFI: 70}:    "evpn",
	{AFI: 16388, SAFI: 71}: "ls",
}

// familyName renders f as the same token subjects.FamilyToken would
// give a matching bgp.Family: the registered name, or the deferred-family
// fallback "x{afi}-{safi}". Nil-safe: f's generated getters return zero
// values for a nil *Family, which lands in the fallback form.
func familyName(f *vantagev1.Family) string {
	af := bgp.Family{AFI: uint16(f.GetAfi()), SAFI: uint8(f.GetSafi())}
	if n, ok := familyNames[af]; ok {
		return n
	}
	return fmt.Sprintf("x%d-%d", af.AFI, af.SAFI)
}

// formatLargeCommunity renders an RFC 8092 large community as
// "global:local1:local2", the community's own canonical text form.
func formatLargeCommunity(lc *vantagev1.LargeCommunity) string {
	return fmt.Sprintf("%d:%d:%d", lc.GetGlobalAdmin(), lc.GetLocalData1(), lc.GetLocalData2())
}

func attrsFrom(a *vantagev1.PathAttributes) attrs {
	out := attrs{
		Origin:      uint8(a.GetOrigin()),
		NextHop:     a.GetNextHop(),
		Communities: a.GetCommunities(),
	}
	for _, seg := range a.GetAsPath() {
		out.ASPath = append(out.ASPath, seg.GetAsns()...)
	}
	for _, ec := range a.GetExtendedCommunities() {
		name := bgp.ExtCommName(uint8(ec.GetType()), uint8(ec.GetSubType()))
		if name == "" {
			// No registered name: keep it addressable by its numbers rather
			// than rendering a bare value that cannot be told apart from a
			// route target.
			out.ExtCommunities = append(out.ExtCommunities,
				fmt.Sprintf("%d:%d:%s", ec.GetType(), ec.GetSubType(), ec.GetValue()))
			continue
		}
		out.ExtCommunities = append(out.ExtCommunities, name+":"+ec.GetValue())
		if name == "rt" {
			out.RouteTargets = append(out.RouteTargets, ec.GetValue())
		}
	}
	for _, lc := range a.GetLargeCommunities() {
		out.LargeCommunities = append(out.LargeCommunities,
			formatLargeCommunity(lc))
	}
	// MED and local-pref are proto3 optional: zero is a legitimate value, so
	// only a set field becomes a non-NULL column.
	if a != nil && a.Med != nil {
		v := a.GetMed()
		out.MED = &v
	}
	if a != nil && a.LocalPref != nil {
		v := a.GetLocalPref()
		out.LocalPref = &v
	}
	return out
}

func routeRows(e envelope, r *vantagev1.RouteEvent) Rows {
	var out Rows
	at := attrsFrom(r.GetAttrs())
	fam := familyName(r.GetFamily())

	// An end-of-RIB marker carries no prefixes but must still be recorded:
	// it is how a consumer knows a peer's initial dump finished.
	//
	// It goes to its own table rather than to route_unicast, which is the
	// whole point of eor_events: a marker is a collection artifact, not a
	// route, and while it sat in a route table every query over that table
	// had to remember `end_of_rib = 0` or count it as one. The marker's own
	// family travels with it, so a marker terminating an EVPN or VPN dump no
	// longer has to be filed in the unicast table to be recorded at all.
	//
	// RouteEvent.end_of_rib is untouched on the wire (see
	// proto/vantage/v1/vantage.proto and collector/session.go); only where
	// the writer files it moved.
	//
	// The announced/withdrawn loops below still run. A well-formed marker
	// carries no prefixes, so in practice they add nothing -- but returning
	// early here would silently drop the prefixes of a malformed sender that
	// set the flag on a non-empty UPDATE, trading one kind of wrong row for
	// actual data loss.
	if r.GetEndOfRib() {
		out.Eor = append(out.Eor, EorRow{envelope: e, Family: fam})
	}
	for _, p := range r.GetAnnounced() {
		out.Unicast = append(out.Unicast, UnicastRow{
			envelope: e, attrs: at, Family: fam,
			Prefix: p.GetPrefix(), PathID: p.GetPathId(),
		})
	}
	for _, p := range r.GetWithdrawn() {
		out.Unicast = append(out.Unicast, UnicastRow{
			envelope: e, attrs: at, Family: fam,
			Prefix: p.GetPrefix(), PathID: p.GetPathId(), IsWithdraw: 1,
		})
	}
	for _, p := range r.GetVpnAnnounced() {
		out.Vpn = append(out.Vpn, vpnRow(e, at, fam, p, 0))
	}
	for _, p := range r.GetVpnWithdrawn() {
		out.Vpn = append(out.Vpn, vpnRow(e, at, fam, p, 1))
	}
	for _, p := range r.GetEvpnAnnounced() {
		out.Evpn = append(out.Evpn, evpnRow(e, at, p, 0))
	}
	for _, p := range r.GetEvpnWithdrawn() {
		out.Evpn = append(out.Evpn, evpnRow(e, at, p, 1))
	}
	return out
}

func vpnRow(e envelope, at attrs, fam string, p *vantagev1.VpnPrefix, wd uint8) VpnRow {
	return VpnRow{
		envelope: e, attrs: at, Family: fam,
		Prefix: p.GetPrefix(), PathID: p.GetPathId(),
		RD: p.GetRd(), Labels: p.GetLabels(), IsWithdraw: wd,
	}
}

func evpnRow(e envelope, at attrs, r *vantagev1.EvpnRoute, wd uint8) EvpnRow {
	return EvpnRow{
		envelope: e, attrs: at,
		RouteType: uint8(r.GetRouteType()), RD: r.GetRd(),
		Prefix: r.GetPrefix(), MAC: r.GetMac(), IP: r.GetIp(),
		GatewayIP: r.GetGatewayIp(), EthernetTag: r.GetEthernetTag(),
		ESI: r.GetEsi(), Labels: r.GetLabels(),
		PathID: r.GetPathId(), IsWithdraw: wd,
	}
}

// familyNamesOf renders a capability's family list with the same registry
// every route row's `family` column uses. Returns a non-nil empty slice, not
// nil: the column is Array(LowCardinality(String)) and an empty array is the
// honest rendering of "negotiated nothing here", which is a different claim
// from a NULL this column cannot hold anyway.
func familyNamesOf(fs []*vantagev1.Family) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, familyName(f))
	}
	return out
}

func peerRow(e envelope, sysDescr string, p *vantagev1.PeerEvent) PeerRow {
	kind := "unspecified"
	switch p.GetKind() {
	case vantagev1.PeerEvent_KIND_UP:
		kind = "up"
	case vantagev1.PeerEvent_KIND_DOWN:
		kind = "down"
	case vantagev1.PeerEvent_KIND_VIEW_LOST:
		kind = "view_lost"
	}
	var fourByte uint8
	if p.GetCaps().GetFourByteAs() {
		fourByte = 1
	}
	caps := p.GetCaps()
	return PeerRow{
		envelope: e, Kind: kind, LocalIP: clickHouseIPv6(p.GetLocalIp()),
		LocalPort: uint16(p.GetLocalPort()), RemotePort: uint16(p.GetRemotePort()),
		DownReason: p.GetDownReason(), CapFourByteAS: fourByte,
		MPFamilies:      familyNamesOf(caps.GetMpFamilies()),
		AddPathFamilies: familyNamesOf(caps.GetAddpathFamilies()),
		HoldTime:        uint16(caps.GetHoldTime()),
		HoldTimeSeen:    boolToUint8(caps.GetHoldTimeSeen()),
		SysDescr:        sysDescr,
	}
}

// clickHouseIPv6 and clickHouseIPv4 normalize a proto-carried address string
// for ClickHouse's IPv4/IPv6 column types, which reject the empty string
// outright rather than accepting it as "no address" -- an insert into
// peer_events for a Peer Down (which has no local address; see
// PeerEvent.LocalIp, set only by handlePeerUp) fails with "converting  to
// IPv6 is unsupported" and is left permanently unacked, since every
// redelivery hits the same conversion error.
//
// The empty string is semantically honest -- a Peer Down genuinely has no
// local address -- so it must not be invented upstream, at the collector.
// It is normalized here instead, at the row-translation boundary where the
// ClickHouse type constraint actually lives, to each family's conventional
// "no address" value: the unspecified address, "::" for IPv6 and "0.0.0.0"
// for IPv4. This mirrors what a NULL would do: a query filtering for a real
// address (e.g. WHERE local_ip != toIPv6('::')) excludes it naturally,
// rather than the insert failing outright.
//
// Every field that feeds an IPv4/IPv6-typed column in deploy/clickhouse/
// schema.sql is routed through one of these two: router_ip and peer_ip
// (IPv6, all six tables, via commonColumns above), peer_bgp_id (IPv4, all
// six tables, via commonColumns above), and local_ip (IPv6, peer_events
// only, via peerRow above).
func clickHouseIPv6(s string) string {
	if s == "" {
		return "::"
	}
	return s
}

func clickHouseIPv4(s string) string {
	if s == "" {
		return "0.0.0.0"
	}
	return s
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// uint32sToUint8s narrows LsNode.SrAlgorithms (protobuf has no uint8 type,
// so the wire type is uint32) to the DDL's Array(UInt8): SR algorithm
// identifiers are a single byte on the wire (RFC 8665 §3.1), so no
// precision is lost.
func uint32sToUint8s(in []uint32) []uint8 {
	out := make([]uint8, 0, len(in))
	for _, v := range in {
		out = append(out, uint8(v))
	}
	return out
}

// ipString renders a 4- or 16-byte address, and "" for absent -- matching how
// the existing row translation normalizes empty IP-typed columns.
func ipString(b []byte) string {
	if a, ok := netip.AddrFromSlice(b); ok {
		return a.String()
	}
	return ""
}

// mergedUnknownTLVs combines a Node/Link NLRI's own unknown TLVs
// (vantagev1.LsNode.unknown_tlvs / LsLink.unknown_tlvs)
// with the BGP-LS Attribute's shared ones (attrLevel, already keyed the same
// way and shared by every node/link RowsFor builds from one envelope). Both
// are logically distinct on the wire -- see those fields' proto doc comments
// -- but deploy/clickhouse/schema.sql has only one unknown_tlvs column per
// table, so this is where the merge actually happens. Copy-on-write: when
// nlriLevel is empty (the common case), attrLevel is returned unchanged
// rather than allocating a new map per row.
func mergedUnknownTLVs(attrLevel map[uint16]string, nlriLevel map[uint32][]byte) map[uint16]string {
	if len(nlriLevel) == 0 {
		return attrLevel
	}
	out := make(map[uint16]string, len(attrLevel)+len(nlriLevel))
	maps.Copy(out, attrLevel)
	for k, v := range nlriLevel {
		out[uint16(k)] = string(v)
	}
	return out
}

// adjSidValues, adjSidFlags and adjSidWeights each project one field out of
// LsLink.AdjSids into its own slice, index-aligned with the other two: the
// DDL stores adj_sids/adj_sid_flags/adj_sid_weights as three parallel
// Array() columns rather than one Array(Tuple), so entry i of each slice
// must describe the same adjacency SID (RFC 9085 §2.2.1). Flags and Weight
// are uint32 on the wire (protobuf has no uint8) but single bytes in the
// DDL (Array(UInt8)); narrowing here is lossless for the same reason as
// uint32sToUint8s above.
func adjSidValues(sids []*vantagev1.LsAdjacencySid) []uint32 {
	out := make([]uint32, 0, len(sids))
	for _, s := range sids {
		out = append(out, s.GetSid())
	}
	return out
}

func adjSidFlags(sids []*vantagev1.LsAdjacencySid) []uint8 {
	out := make([]uint8, 0, len(sids))
	for _, s := range sids {
		out = append(out, uint8(s.GetFlags()))
	}
	return out
}

func adjSidWeights(sids []*vantagev1.LsAdjacencySid) []uint8 {
	out := make([]uint8, 0, len(sids))
	for _, s := range sids {
		out = append(out, uint8(s.GetWeight()))
	}
	return out
}
