// Package sink turns collector envelopes into ClickHouse rows and inserts
// them. The translation (rows.go) is deliberately pure -- no I/O, no clients,
// no clock -- because that is where schema fidelity is decided and pure
// functions can be tested exhaustively without infrastructure.
package sink

import "time"

// envelope holds the columns every table carries. Embedded rather than
// repeated so a change to the common shape cannot drift between tables.
type envelope struct {
	CollectorID   string
	RouterIP      string
	RouterSysname string
	PeerIP        string
	// RIB names which RIB view this row belongs to: one of the four RFC 8671
	// adj-RIB streams -- "in_pre", "in_post", "out_pre", "out_post" -- or
	// "loc_rib" for RFC 9069 Loc-RIB, the router's own table after best-path
	// selection, which is not a direction at all and which nobody sent us. It
	// is part of every sort key, so routes a router sent can never collapse
	// against routes it received, and a Loc-RIB dump never collapses against
	// either.
	RIB         string
	PeerASN     uint32
	PeerBGPID   string
	SessionID   uint64
	Seq         uint64
	TsRouter    time.Time
	TsCollector time.Time
	ParseFlags  []string
	StreamSeq   uint64
}

// attrs holds the BGP path attributes shared by all three route tables.
type attrs struct {
	Origin           uint8
	ASPath           []uint32
	NextHop          string
	MED              *uint32
	LocalPref        *uint32
	Communities      []uint32
	ExtCommunities   []string
	RouteTargets     []string
	LargeCommunities []string
}

// Field order below does not need to match deploy/clickhouse/schema.sql's
// column order, and does not: envelope and attrs are embedded first here
// for readability, while the DDL puts each table's own columns first. That
// is safe only because clickhouse.go's inserters never reflect over these
// structs -- each names the fields it binds, one Append call per table --
// so do not introduce clickhouse-go's AppendStruct (or any other
// reflection-over-field-order insert path) for these types. AppendStruct
// would silently turn this declaration order into a wire contract against
// the DDL, and the two would drift apart with no compiler error to catch
// it. The DDL is the source of truth for column order; these structs are
// not.
//
// Those Append calls are themselves positional -- the argument list has to
// match the DDL column for column, and nothing in Go's type system connects
// the two (see the note above insertUnicast in clickhouse.go, and
// TestInsertRoundTrip, which is what actually catches a mismatch). Writing
// each field out at the call site is what keeps the correspondence legible
// to a reader; it is not a named binding, and it is not checked.
type UnicastRow struct {
	envelope
	attrs
	Family     string
	Prefix     string
	PathID     uint32
	IsWithdraw uint8
}

// EorRow is one BGP End-of-RIB marker, bound for eor_events.
//
// It has no attrs and no prefix because a marker has neither: it is an
// empty UPDATE, a protocol sentinel saying "the initial dump for this
// (peer, family) is complete". It used to be filed as a UnicastRow with
// EndOfRIB = 1, which meant route_unicast held rows that were not routes --
// see eor_events' comment in deploy/clickhouse/schema.sql for the two
// defects that shape caused. Family is the column that makes dump progress
// answerable for a peer that only ever carried VPN or EVPN.
type EorRow struct {
	envelope
	Family string
}

type VpnRow struct {
	envelope
	attrs
	Family     string
	Prefix     string
	PathID     uint32
	IsWithdraw uint8
	RD         string
	Labels     []uint32
}

type EvpnRow struct {
	envelope
	attrs
	RouteType   uint8
	RD          string
	Prefix      string
	MAC         string
	IP          string
	GatewayIP   string
	EthernetTag uint32
	ESI         string
	Labels      []uint32
	PathID      uint32
	IsWithdraw  uint8
}

type LsRow struct {
	envelope
	Family     string
	RawReach   []byte
	RawUnreach []byte
	// EndOfRIB: an LS End-of-RIB has no raw bytes at all,
	// so this is the only signal on this row that distinguishes it from
	// (say) a zero-length raw remainder. It stays a field here rather than
	// moving to EorRow with the BGP markers: ls_events is not a route table,
	// so a query over it was never at risk of counting a marker as a route.
	EndOfRIB uint8
}

// LsNodeRow is one BGP-LS node NLRI (RFC 9552 §5.2). node_key is
// MATERIALIZED in the DDL from (protocol, identifier, asn, bgpls_id, area,
// router_id) -- it is not a field here and must never be passed to Append.
type LsNodeRow struct {
	envelope
	Protocol   uint8
	Identifier uint64
	ASN        uint32
	BgplsID    uint32
	Area       uint32
	RouterID   string
	// RouterIDv4 is the node's IPv4 router-id, decoded
	// from BGP-LS Attribute TLV 1028 and rendered dotted-quad, "" when
	// absent. RouterID above stays hex/raw -- IS-IS system IDs are 6 bytes,
	// not addresses -- so this is a supplement, not a replacement: without
	// it, topology node labels rendered as hex, defeating the whole point
	// of decoding a dotted-quad router ID at all.
	RouterIDv4   string
	IsWithdraw   uint8
	Name         string
	SrgbBase     uint32
	SrgbSize     uint32
	SrlbBase     uint32
	SrlbSize     uint32
	SrAlgorithms []uint8
	UnknownTLVs  map[uint16]string
}

// LsLinkRow is one BGP-LS link NLRI (RFC 9552 §5.3). local_node_key and
// remote_node_key are MATERIALIZED the same way node_key is above, from the
// local/remote descriptor tuples -- neither is a field here. AdjSIDs,
// AdjSIDFlags and AdjSIDWeights are parallel arrays: index i of each
// describes the same adjacency SID (RFC 9085 §2.2.1), so a translation that
// appends to one must append to all three in lockstep or the columns
// misalign.
type LsLinkRow struct {
	envelope
	Protocol       uint8
	Identifier     uint64
	LocalASN       uint32
	LocalBgplsID   uint32
	LocalArea      uint32
	LocalRouterID  string
	RemoteASN      uint32
	RemoteBgplsID  uint32
	RemoteArea     uint32
	RemoteRouterID string
	LocalIfAddr    string
	RemoteIfAddr   string
	LinkLocalID    uint32
	LinkRemoteID   uint32
	IsWithdraw     uint8
	AdjSIDs        []uint32
	AdjSIDFlags    []uint8
	AdjSIDWeights  []uint8
	TEMetric       uint32
	IGPMetric      uint32
	AdminGroup     uint32
	MaxBandwidth   float32
	UnknownTLVs    map[uint16]string
}

// LsPrefixRow is one IPv4 Topology Prefix NLRI. Prefix is rendered text
// ("10.255.0.5") rather than the hex RouterID uses: a prefix is always an
// address, so there is nothing to lose by rendering it and a looking-glass
// query is unusable against hex.
type LsPrefixRow struct {
	envelope
	Protocol        uint8
	Identifier      uint64
	ASN             uint32
	BgplsID         uint32
	Area            uint32
	RouterID        string
	Prefix          string
	PrefixLen       uint8
	IsWithdraw      uint8
	PrefixSID       uint32
	PrefixSIDFlags  uint8
	HasPrefixSID    uint8
	PrefixMetric    uint32
	PrefixAttrFlags uint8
	OSPFRouteType   uint8
	UnknownTLVs     map[uint16]string
}

type PeerRow struct {
	envelope
	Kind          string
	LocalIP       string
	LocalPort     uint16
	RemotePort    uint16
	DownReason    uint32
	CapFourByteAS uint8

	// The session facts the wire has always carried. Three of the four
	// needed no decoder change -- see peer_events' own comment in
	// deploy/clickhouse/schema.sql for what each was being dropped by.
	//
	// MPFamilies and AddPathFamilies are rendered with familyName, the SAME
	// registry the route tables' own `family` column uses. A capability list
	// saying "ipv4" where a route row says "ipv4u" would be unjoinable while
	// both looked right.
	MPFamilies      []string
	AddPathFamilies []string

	// HoldTimeSeen is not redundant with HoldTime != 0. RFC 4271 sec 4.2: a
	// hold time of 0 means the timer never expires -- keepalives off for the
	// session -- so 0 is a real negotiated value. A Peer Down carries no OPEN
	// and so reports no hold time at all. Both are the number 0 and only
	// this flag separates them.
	//
	// There is no keepalive field, here or anywhere downstream: the OPEN
	// does not carry one, and hold_time/3 is a convention rather than an
	// observation.
	HoldTime     uint16
	HoldTimeSeen uint8

	// SysDescr is the Initiation sysDescr TLV (type 1) -- what the router
	// says it IS. envelope.RouterSysname is the type-2 sysName, what it
	// calls itself. Different TLVs; this archive stored only the second.
	SysDescr string
}

type StatsRow struct {
	envelope
	Counters map[uint32]uint64
}

// BeatRow is one CollectorBeat, bound for collector_beats. It embeds no
// envelope: a beat names a collector and nothing else -- no router, peer,
// session or seq -- so the thirteen envelope columns would be thirteen zeros.
// inserted_at is not here either. ClickHouse fills it at insert (DEFAULT
// now64(3)), and it has to be the server's clock, never the collector's or
// the writer's; see collector_beats in schema.sql.
type BeatRow struct {
	CollectorID string
	StartedAt   time.Time
	BeatAt      time.Time
}

// Rows is everything one envelope produced. All slices may be empty: an
// end-of-RIB marker or a withdraw-only update legitimately yields no rows in
// some tables and rows in others.
type Rows struct {
	Unicast    []UnicastRow
	Vpn        []VpnRow
	Evpn       []EvpnRow
	Eor        []EorRow
	Ls         []LsRow
	LsNodes    []LsNodeRow
	LsLinks    []LsLinkRow
	LsPrefixes []LsPrefixRow
	Peer       []PeerRow
	Stats      []StatsRow
	Beats      []BeatRow
}

// Len is the total row count across all tables, used for batch sizing.
func (r Rows) Len() int {
	return len(r.Unicast) + len(r.Vpn) + len(r.Evpn) + len(r.Eor) +
		len(r.Ls) + len(r.LsNodes) + len(r.LsLinks) + len(r.LsPrefixes) +
		len(r.Peer) + len(r.Stats) + len(r.Beats)
}

// Add accumulates one envelope's rows into a batch.
//
// This exists because the consumer used to inline these appends, and when
// Rows grew LsNodes and LsLinks the inlined list was not grown with it: the
// two typed BGP-LS tables were computed by RowsFor and then dropped on the
// floor, while Insert returned nil over the empty slices and the envelope
// was acked as durably stored. Nothing failed and the topology was lost.
// Every field-by-field walk of Rows now lives in this file, next to Len,
// so the two enumerations that must stay exhaustive are read together --
// a new row type that misses one is very hard to add without noticing the
// other. See TestRowsAddCoversEveryRowSlice, which fails on any field this
// method forgets.
func (r *Rows) Add(o Rows) {
	r.Unicast = append(r.Unicast, o.Unicast...)
	r.Vpn = append(r.Vpn, o.Vpn...)
	r.Evpn = append(r.Evpn, o.Evpn...)
	r.Eor = append(r.Eor, o.Eor...)
	r.Ls = append(r.Ls, o.Ls...)
	r.LsNodes = append(r.LsNodes, o.LsNodes...)
	r.LsLinks = append(r.LsLinks, o.LsLinks...)
	r.LsPrefixes = append(r.LsPrefixes, o.LsPrefixes...)
	r.Peer = append(r.Peer, o.Peer...)
	r.Stats = append(r.Stats, o.Stats...)
	r.Beats = append(r.Beats, o.Beats...)
}
