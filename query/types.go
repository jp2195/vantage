package query

import (
	"net/netip"
	"time"
)

// unmapAll unmaps every address in addrs in place, so that every netip.Addr
// this package returns -- IP, RouterIP, PeerIP and NextHop alike -- is in
// the same plain form regardless of which column or parse produced it.
//
// Two separate paths can hand back the mapped form, and both are live.
// clickhouse-go scans an IPv6-typed column (router_ip, peer_ip) into a
// netip.Addr via netip.AddrFrom16, which never unmaps. And netip.ParseAddr,
// which is how this package's own NextHop fields are produced from the
// next_hop String column, returns the mapped form for mapped TEXT:
// ParseAddr("::ffff:10.0.103.61") is Is4In6, prints mapped, and compares
// unequal to the plain address. Every scan loop here therefore passes its
// next hop through as well -- the comment that used to sit at each of those
// four call sites claimed ParseAddr "never produces the mapped form to
// begin with", which is false, and the repo's own decoder writes exactly
// that text (bgp/update.go's and bgp/nlri.go's netip.AddrFrom16 paths).
//
// Left uncorrected, one route row could carry "::ffff:10.0.103.61" in
// router_ip and "10.0.103.61" in next_hop -- two textual forms of one
// address in the same JSON object. That divergence is not cosmetic:
// netip.Addr equality is representation-sensitive, so route.NextHop ==
// peer.PeerIP would be unconditionally false for every IPv4 pair, which is
// exactly the comparison a looking glass needs to make.
//
// A zero Addr unmaps to the zero Addr, so a field a failed parse left
// invalid passes through untouched rather than becoming a different kind of
// wrong.
func unmapAll(addrs ...*netip.Addr) {
	for _, a := range addrs {
		*a = a.Unmap()
	}
}

// Router is one row of Routers: a router's newest BMP session and how many
// of the peers still counted in that session are up versus down as of its
// end. SessionID and LastSeen both describe that same current session --
// neither is "ever seen", both are "as of the current one" -- because that
// is the only reading peerStateCTE supports; see its doc comment for why
// the other reading (any session, ever) is a defect and not a looser
// version of this one.
//
// PeersUp and PeersDown count PEERS, not the RIB views a router mirrors for
// them: a peer the router mirrors pre- and post-policy is one peer here,
// with one state -- its newest event across every rib. They are resolved
// from peer_up rather than peer_state for that reason; see routersSQL for
// what the rib-grained count reported instead.
//
// PeersUp + PeersDown + PeersViewLost is the router's peer count whenever
// every peer's newest event resolved to one of those three. It is not an
// identity: peer_events.kind also carries 'unspecified', which sink writes
// for a PeerEvent whose kind it does not recognize, and such a peer is in
// none of the three -- so the sum can be below the peer count and there is
// no field here that names the difference. Zero unspecified rows in the
// archive today; see routersSQL.
//
// IP is always in plain form -- "10.0.103.61", never "::ffff:10.0.103.61"
// -- for an IPv4 router; see unmapAll's own doc comment for why that is not
// automatic.
//
// Collector is the collector whose view this row is, and it is part of the
// row's identity rather than a label on it: session identity is (collector,
// router), never router alone. One collector per router is the supported
// deployment, and this field is what keeps the unsupported case legible
// instead of silent -- a router two collectors both monitor yields one
// Router per collector, each carrying that collector's own session, peer
// counts and last-seen, rather than a merged answer or (what this package
// did before) whichever collector's clock was momentarily ahead, with the
// other's entire view discarded unreported. Peer, Route and VPNRoute all
// carry the same field, with the same meaning. See peerStateCTE's doc
// comment for the defect this prevents and api/openapi.yaml's
// Router.collector for the same statement in the HTTP contract.
type Router struct {
	SysName   string
	IP        netip.Addr
	Collector string
	SessionID uint64
	PeersUp   int
	PeersDown int
	// PeersViewLost counts peers whose current session ended with the
	// COLLECTOR losing its BMP transport rather than with the ROUTER
	// reporting the BGP session down. It is a third bucket rather than a
	// second name for PeersDown because those are different facts about
	// different subjects, and because a collector that dies makes every one
	// of a router's peers view-lost at once: folding them into PeersDown
	// would report a healthy router as having lost every BGP session it has.
	//
	// Its usual value is 0, and a non-zero one is worth alerting on: it says
	// nobody is currently watching these peers. See collector's
	// Session.Close for where the underlying event comes from.
	PeersViewLost int
	// PeersStale counts peers whose stored state is up but whose collector
	// has not been heard from within the stale threshold. Their routes are
	// still served (see servedGate), and StalePairs names their sessions so
	// the API layer can mark the answers that carry them. Normally 0; every
	// peer of a collector reads stale at once when it, NATS or the writer
	// stalls.
	PeersStale int
	LastSeen   time.Time
}

// Peer is one row of Peers: one (collector, router, peer, rib) tuple's
// state as of that (collector, router)'s current BMP session -- the same
// "current" peerStateCTE's doc comment defines for Router. State is the
// state the session ended in, not whether the peer went down at some point
// during it; a peer that flapped down and came back up within its current
// session still reports up. See Peers' own doc comment for why, and the
// live archive's own shape (12 of its 17 down events are followed by an up
// in the same session) for why that distinction has to be tested against a
// manufactured fixture rather than the archive itself.
//
// DumpStates is per-family initial-dump progress: family name ("ipv4u",
// "vpn4", "evpn", ...) to one of two plain strings, not a bool and not a
// dedicated type -- this package's callers include an LLM reading JSON, and
// named values are more legible there than a boolean whose meaning depends
// on reading the field's doc comment first. api/openapi.yaml's dump_states
// schema is the contract this map answers to.
//
//   - "complete" -- the current session carries an eor_events row for that
//     (router, peer, rib, family): the initial table dump for that family
//     has finished.
//   - "dumping" -- the current session carries route rows for the family
//     and no marker for it: the dump is genuinely still in progress. This
//     is the only value that means that.
//
// A family with neither is ABSENT from the map rather than present as
// "unknown": a session that carried no rows at all for a family has no
// dump to be part-way through, and an entry claiming otherwise would be
// manufacturing a fact about a family the router never mentioned. Callers
// distinguish "not started or not carried" from "in progress" by the key
// being missing, not by a third value.
//
// The map is EMPTY for a down peer, whatever markers its current session
// has on record: a peer that finished dumping earlier in the session and
// later went down is not "still dumping", and it is not "complete" either
// -- it is disconnected, and its dump progress is not a meaningful
// question. peersSQL checks peer state before any family logic for exactly
// this reason; see dumpStatesExpr and
// TestPeersReportsDumpStateForADownPeerWithACompleteDump.
//
// This map replaces a single string that could only ever describe unicast:
// end_of_rib used to live on route_unicast and on no other route table, so
// a peer carrying only VPN or EVPN routes read "unknown" forever no matter
// how complete its dump was -- there was no way to ask the question. Moving
// the markers into eor_events, with a family column, is what made it
// askable.
//
// It is askable for four of the five families. "ls" never appears here,
// whatever a session carried, because an LS End-of-RIB is still filed as an
// ls_events row rather than an eor_events one -- see peersSQL's KNOWN GAP
// paragraph for why that is a writer decision rather than something this
// map can close, and for the trigger that turns it into a defect.
//
// Routes is the deduped count of prefixes (really (prefix, path_id) route
// keys -- see Route's own doc comment for why path_id is part of the
// identity) this peer is currently advertising: live in its current
// session and not withdrawn. It counts unicast routes only, unlike
// DumpStates above, which spans every family. It is zero for a
// down peer, the same "a down peer contributes nothing" rule Routes(ctx,
// prefix) already enforces, and it is not simply route_unicast's row
// count for this (router, peer, rib) -- see peersSQL's own doc comment
// for why a plain count() is wrong here even after every other predicate
// is right: ReplacingMergeTree's redelivery guarantee only becomes true
// at the next merge, and until then a redelivered envelope's rows are
// still two rows, not one.
//
// Collector is the collector whose view this row is, the same field and the
// same guarantee Router.Collector documents: session identity is
// (collector, router), so a peer on a router two collectors both monitor
// yields one Peer per collector -- each with that collector's own session,
// state, dump progress and route count -- rather than a merged row.
//
// RouterIP and PeerIP are always in plain form -- see unmapAll's own doc
// comment -- so an IPv4 peer's PeerIP is directly comparable to, say, a
// Route.NextHop carrying the same address, rather than differing by the
// IPv4-mapped ::ffff: prefix clickhouse-go's raw IPv6 scan would otherwise
// leave in one of the two and not the other.
type Peer struct {
	RouterIP   netip.Addr
	PeerIP     netip.Addr
	Collector  string
	RIB        string
	ASN        uint32
	State      string
	DumpStates map[string]string
	SessionID  uint64
	Routes     int

	// The session facts, as of the newest event in this session that
	// actually carried them. NOT the newest event: a Peer Down carries no
	// OPEN, so a peer that came up and later flapped has its facts on the
	// older row, and argMax over every event would report a session that
	// negotiated nothing -- indistinguishable on screen from one that did.
	//
	// HoldTimeSeen is what separates "negotiated a hold time of 0", which
	// RFC 4271 §4.2 defines as a timer that never expires, from "no OPEN was
	// observed in this session" -- which is true of every peer whose only
	// event is a Down.
	//
	// There is no keepalive field, here or anywhere: the OPEN does not
	// carry one.
	HoldTime     uint16
	HoldTimeSeen bool

	// UpSince is when this peer's CURRENT session last came up, on the
	// collector clock, and the zero instant when the session never carried
	// an up at all.
	//
	// IT IS NOT AN UPTIME, and the difference is the whole reason it is an
	// instant. A measurement on 2026-09-21 compared `now - up_since`
	// against this archive: for 25 of 35 peers it equaled how long the
	// router had been SILENT, to the decimal -- the environment that
	// produced this archive was torn down rather than shut down, so no
	// Peer Down was ever sent and the last state stays
	// `up` forever. A duration would render archive silence as a live
	// session. The instant is a fact; the duration is an inference about the
	// present that this archive cannot support.
	UpSince time.Time

	// MPFamilies is what the two speakers NEGOTIATED, which is a different
	// and better answer than the families a peer happens to have routes in:
	// a family negotiated and carrying nothing is visible here and invisible
	// to a row count. Rendered with the same family vocabulary the route
	// tables' own `family` column uses.
	MPFamilies      []string
	AddPathFamilies []string

	// SysDescr is the router's Initiation sysDescr TLV (type 1) -- what it
	// says it IS. It is a router fact carried on a peer row because
	// peer_events is where this pipeline denormalizes router identity; the
	// sysName equivalent has always lived there as router_sysname.
	SysDescr string
}

// Route is one row of Routes: one (collector, router, peer, rib, path_id)
// currently advertising Prefix, as of that (collector, router)'s current
// BMP session and that peer's current up state -- the same "current"
// peerStateCTE's doc comment defines for Router and Peer.
//
// RouterIP, PeerIP and NextHop are all in plain form -- see unmapAll's own
// doc comment -- even though they come from two different places: RouterIP
// and PeerIP are scanned from route_unicast's IPv6-typed columns, while
// NextHop is parsed from a plain String column via netip.ParseAddr. Both
// paths can yield the IPv4-mapped form, so routesSQL's scan loop unmaps all
// three. A caller comparing NextHop to a Peer.PeerIP or Router.IP from
// elsewhere in this package sees the same address compare equal either way.
//
// PathID is part of the route's identity, not incidental data: add-path
// lets one peer advertise the same prefix under several path-ids at once,
// each independently withdrawable. Two Routes that agree on RouterIP,
// PeerIP, RIB and Prefix but differ in PathID are two distinct routes, not
// one route reported twice -- see routesSQL's doc comment and TestRoutes's
// "add-path keeps both path-ids" subtest, which exists because grouping on
// prefix alone -- the obvious shape -- silently keeps one sibling and
// drops the rest.
//
// ASPath and OriginASN are two different claims, and callers need both to
// tell them apart. ASPath is the AS path exactly as received, which is
// ordinary to find empty on route_unicast too: an iBGP route, or a peer
// whose feed simply never carries AS-path information, both look the same
// as "the peer reflected this route to us without adding itself to the
// path." OriginASN is the last hop of that path, and it is only
// meaningful when ASPath is non-empty -- callers must check
// len(ASPath) == 0 before trusting it, because OriginASN's zero value and
// the reserved AS 0 are the same bit pattern with no way to tell them
// apart on their own.
//
// This hazard was proven on route_vpn, not route_unicast: 142 of the
// archive's 164 VPN rows carry no AS path, and deriving origin as
// as_path[-1] in SQL returned the reserved AS 0 for every one of them --
// indistinguishable from a route that genuinely originated there -- until
// l3vpn-rib-browser.json was fixed on 2026-08-23 to tell the two apart.
// route_unicast's own as_path column is the identical type with the
// identical failure mode available to it; OriginASN is derived in Go here
// so this package does not have to reproduce that defect on this table
// before it earns the same fix.
//
// DumpState is a single string, not Peer.DumpStates' map, and it is scoped
// to THIS ROW'S OWN family: a route is only interested in whether the dump
// that would carry its siblings has finished, not in how some other family
// on the same session is doing. It is one of "complete" (the session
// carries an eor_events marker for this route's (router, peer, rib,
// family)), "dumping" (it does not, but this route proves the family has
// rows), or "unknown" (the peer is down, or peer_events has no row for this
// route's rib at all, so there is nothing to conclude) -- see dumpStateExpr,
// which derives it.
//
// Collector is the collector whose view this row is, the same field and the
// same guarantee Router.Collector documents: session identity is
// (collector, router), so a route learned on a router two collectors both
// monitor is reported once per collector -- each row resolved against that
// collector's own current session and its own peer state -- rather than
// merged into one row or dropped with the losing collector's whole view.
//
// It exists on Route, not only on Peer, because an empty Routes result is
// ambiguous by itself -- "this prefix is nowhere in the network" and "the
// peer that would carry it has not finished its initial table dump yet"
// produce the identical empty slice. DumpState on the rows Routes does
// return is what tells a caller whether the (router, peer, rib) behind a
// route it is looking at might still have more of that family's table to
// deliver.
//
// Family is route_unicast's own family column -- "ipv4u" or "ipv6u", the
// tokens subjects.FamilyToken produces -- read off the row rather than
// inferred from Prefix's shape. api/openapi.yaml puts family in
// UnicastRoute.required, and it is also the column DumpState is resolved
// against (routesSQL joins eor ON eor.fam = r.family), so a caller told
// "this route's dump is still in progress" can see which family that claim
// is about. It is in routesSQL's GROUP BY as well as its SELECT list, which
// is what keeps any(dumpStateExpr) constant within a group by construction
// rather than by a property of how prefixes happen to be spelled; see
// routesSQL's own doc comment.
//
// MED and LocalPref are POINTERS, and they are pointers for the reason
// OriginASN needs ASPath beside it and VPNRoute's Label needs HasLabel: a
// value whose zero is a real value cannot also use that zero to mean
// "absent". Both columns are Nullable(UInt32) in ClickHouse and
// [integer, "null"] in the contract, and MED 0 -- which a router really does
// advertise, and which wins a tie-break against a route carrying MED 10 --
// is a different fact from "this route carries no MED at all". A pointer
// keeps the two apart the whole way: nil marshals to JSON null, a pointer to
// 0 marshals to 0, and no caller has to remember to check a companion bool
// first. It is a pointer rather than a MED/HasMED pair only because the
// storage column has a NULL to carry the distinction, where a label dug out
// of an Array(UInt32) has none -- see label(). sink's own attrs struct types
// these same two columns *uint32 (proto3 optional in, Nullable out), so the
// representation is unbroken from the BMP wire to here.
//
// Communities is RFC 1997 communities in the "65000:100" notation
// api/openapi.yaml asks for, rendered from the stored Array(UInt32) by
// communityStrings in Go rather than by an expression in the statement --
// see that function's own doc comment for why that side of the line is the
// safe one. LargeCommunities is passed through untouched: sink already
// writes RFC 8092's own "global:local1:local2" text into an Array(String),
// so there is nothing here to render and nothing to get wrong. Both are
// empty rather than nil on a route that carries none, matching what
// clickhouse-go's own scan produces for an empty array, so neither field can
// reach a JSON encoder as null where the contract says array.
type Route struct {
	RouterSysName             string
	RouterIP, PeerIP, NextHop netip.Addr
	Collector                 string
	RIB, Family, Prefix       string
	PathID                    uint32
	ASPath                    []uint32
	OriginASN                 uint32
	MED                       *uint32
	LocalPref                 *uint32
	Communities               []string
	LargeCommunities          []string
	// ExtCommunities and RouteTargets are rendered text, not numbers, and
	// the two overlap by design: route_targets is the rt: subset of
	// ext_communities stored again with its prefix stripped, in the form a
	// person types. Both are returned because community= searches both, and
	// a filter must never match on a column the response omits.
	ExtCommunities []string
	RouteTargets   []string
	DumpState      string
}

// VPNRoute is one row of VPNRoutes: one (collector, router, peer, rib, rd,
// prefix, path_id) currently advertising Prefix under route distinguisher
// RD, as of that (collector, router)'s current BMP session and that peer's
// current up state --
// the same "current" peerStateCTE's doc comment defines for Route. Like
// Routes, VPNRoutes excludes a route whose newest observation was a
// withdrawal (see vpnRoutesSQL's own HAVING live_is_withdraw = 0, and its
// doc comment for why omitting that clause is wrong);
// VPNRoute carries no is_withdraw field because, unlike Route, nothing here
// needs to report a withdrawn route's other fields alongside it in the
// first place.
//
// DumpState is the same field, the same three values and the same per-family
// scoping Route.DumpState documents -- read it there rather than have two
// accounts of one rule. It is on this struct because api/openapi.yaml puts
// dump_state in RouteCommon.required and VPNRoute is an allOf over
// RouteCommon, and because the underlying question only became answerable
// per family when end-of-RIB markers moved into eor_events: while they lived
// on route_unicast alone, a peer carrying nothing but VPN routes had no dump
// progress to report at all. A partially-delivered VPN table reported as a
// complete one is exactly the confidently-wrong answer this field exists to
// prevent, and it is why vpnRoutesSQL grew a LEFT JOIN peer_state and a LEFT
// JOIN eor without letting either decide whether a route survives.
//
// RouterIP, PeerIP and NextHop are all in plain form, the same guarantee
// and the same reason Route's own doc comment gives -- see unmapAll's own
// doc comment.
//
// Collector is the collector whose view this row is, the same field and the
// same guarantee Router.Collector documents: session identity is
// (collector, router), so a VPN route on a router two collectors both
// monitor is reported once per collector rather than merged into one row.
//
// RIB and PathID are part of the route's identity, not incidental data,
// for the identical reason Route's own doc comment gives for PathID:
// vpnRoutesSQL groups by (collector_id, router_ip, peer_ip, rib, rd,
// prefix, path_id), and two rows that agree on every other field but
// differ in rib or path_id are two distinct routes -- an in_pre
// observation and an out_pre one, or two add-path siblings -- not one route
// reported twice. Without these two fields, the query would still be
// correct, but two such siblings would come back identical in every field
// VPNRoute exposes, which is indistinguishable from the query having
// collapsed them. See TestVPNRoutes' "add-path
// keeps both path-ids distinguishable" subtest.
//
// RD is the empty string for a family that carries no route distinguisher
// at all -- lu4 (BGP-LU) is 74 of the live archive's 164 route_vpn rows,
// every one of them with rd = "" -- not a sentinel or a rendered
// placeholder for that state. See vpnRoutesSQL's own doc comment for why
// the column is aliased vrf rather than rd in the query that produces
// this field, and TestVPNRoutes' "an RD-less route reports the real
// column, not a placeholder" subtest for what would go wrong if it ever
// were rendered before landing here: l3vpn-rib-browser.json once rendered
// this same RD-less family as "(none: this family carries no RD)" and
// then filtered on that rendered value instead of the real column, which
// made the family invisible to its own filter.
//
// ASPath and OriginASN are two different claims, the same pair Route
// exposes and for the same reason (see Route's own doc comment): ASPath
// is the AS path exactly as received -- ordinary to find empty here too,
// 142 of the live archive's 164 route_vpn rows carry no AS path at all --
// and OriginASN is only meaningful once a caller has checked
// len(ASPath) == 0, because its zero value and the reserved AS 0 are the
// same bit pattern with no way to tell them apart on their own. This is
// the very hazard that was first proven on route_vpn, not route_unicast:
// deriving origin as as_path[-1] in SQL returned the reserved AS 0 for
// every one of those 142 rows, indistinguishable from a route that
// genuinely originated there, until l3vpn-rib-browser.json was fixed on
// 2026-08-23 to tell the two apart. OriginASN is derived here
// via originASN, the same function routes.go uses, rather than a second
// copy of the fix for the same defect.
//
// Label and HasLabel are two fields for the reason OriginASN and ASPath
// are two fields on Route: 0 is both "the implicit-null label" (a real,
// meaningful value an MPLS router advertises) and the zero value an unset
// uint32 already has, so Label alone cannot tell a caller which one it is
// looking at. HasLabel is false in two cases: the underlying route's
// newest observation carried 524288 -- 0x800000 >> 4, RFC 3107's own
// withdraw sentinel -- in the label field a real label would otherwise
// occupy, or that observation's labels array was empty outright, which
// ClickHouse's own out-of-range array indexing cannot be trusted to
// surface: labels[1] on an empty array returns the element type's zero
// value, 0, exactly the value a real implicit-null label carries, so a
// route with no label at all and a route genuinely labeled 0 would
// otherwise be the same, unresolvable ambiguity HasLabel exists to remove.
// vpnRoutesSQL selects the whole labels array rather than its first
// element for exactly this reason; label() (see its own doc comment)
// inspects the array itself rather than a subscript that already discarded
// the "empty" case before label() ever saw it. On the live archive both
// paths to HasLabel = false are defense in depth rather than the primary
// guard: every row currently carrying the sentinel is also a withdrawal,
// and no row carries an empty labels array at all, so VPNRoutes excludes
// the sentinel case outright before Label is ever computed (see
// vpnRoutesSQL's own doc comment) and has no fixture-free way to exercise
// the empty-array case at all -- see TestVPNRoutes' "a route with no
// labels reports no label, not label 0" subtest, which is fixture-only
// for that reason.
//
// RouteTargets is empty, not an error, on a route that has none: 108 of
// the live archive's 164 route_vpn rows carry no route target at all
// (every lu4 row, which is not a VPN family and has nowhere to put one,
// plus 34 of the 90 vpn4 rows that predate the extended-communities
// decoder, which landed 2026-08-10). An inner join to a route-target table
// would drop those rows from the answer entirely rather than report them
// with an empty RouteTargets; VPNRoutes does neither, the same way
// Routes never lets an unparseable next_hop cost a caller the rest of
// that row.
//
// Family is route_vpn's own family column -- "vpn4", "vpn6" or "lu4" -- and
// it matters more here than the same field does on Route, because this one
// table holds all three at once. api/openapi.yaml puts family in
// VPNRoute.required, and RD alone does not recover it: an lu4 route and a
// VPN route are told apart by the family column, not by whether RD happens
// to be empty. It is in vpnRoutesSQL's GROUP BY as well as its SELECT list
// for the reason that statement's own doc comment gives -- any(dumpStateExpr)
// resolves against eor.fam = r.family, and a group must not be able to mix
// two families for that expression to choose between.
//
// MED, LocalPref, Communities and LargeCommunities are the same four fields,
// with the same two nullable columns and the same Go-side community
// rendering, that Route carries -- read Route's own doc comment rather than
// have two accounts of one decision.
type VPNRoute struct {
	RouterSysName             string
	RouterIP, PeerIP, NextHop netip.Addr
	Collector                 string
	RIB                       string
	Family                    string
	RD, Prefix                string
	PathID                    uint32
	Label                     uint32
	HasLabel                  bool
	RouteTargets              []string
	ASPath                    []uint32
	OriginASN                 uint32
	MED                       *uint32
	LocalPref                 *uint32
	Communities               []string
	LargeCommunities          []string
	// ExtCommunities is the same field and the same reason Route's own doc
	// comment gives; VPNRoute already carries RouteTargets, the rt: subset
	// of it, because the statement already selects r.route_targets AS
	// live_route_targets.
	ExtCommunities []string
	DumpState      string
}

// EVPNRoute is one row of EVPNRoutes: one live EVPN NLRI as of its
// (collector, router)'s current BMP session and its peer's current up state
// -- the same "current" peerStateCTE's doc comment defines for Route and
// VPNRoute, and the same exclusion of a route whose newest observation was a
// withdrawal.
//
// Its identity is the whole NLRI tuple -- RouteType, RD, Prefix, MAC, IP,
// EthernetTag, ESI, PathID -- not a prefix. That is what makes this a third
// route family rather than route_vpn with different columns: an EVPN type-2
// MAC/IP route has no prefix at all, and two rows differing only in ESI are
// two routes for the identical reason two rows differing only in PathID are
// (see Route.PathID). The live archive's 202 route_evpn rows collapse to 16
// distinct tuples, so a query that dropped any component of the key would
// still return plausible-looking rows.
//
// Prefix, MAC and IP are "" on the route types that do not carry them, and
// that is ordinary rather than missing data: on the live archive every
// type-3 (IMET) row has an empty prefix, MAC, IP and ESI, every type-2 row
// has a MAC and no prefix, and every type-5 row has a prefix and no MAC.
// A type-3 route reduced to its RD and EthernetTag is still a route, and a
// query that treats an empty MAC as a row to skip loses 64 of the archive's
// 202 rows -- see TestEVPNRoutes' own IMET subtest.
//
// RouteType is the bare wire value, a UInt8, with no name mapping in this
// package. api/openapi.yaml documents 1 Ethernet A-D, 2 MAC/IP, 3 IMET,
// 4 ES, 5 IP prefix; the archive carries 2, 3 and 5 only, so any mapping
// this package supplied for 1 and 4 would ship with no fixture and no live
// row behind it. Callers that want names can read them off the contract,
// where the claim is documentation rather than an untested branch. Note also
// that bgp/evpn.go stores the real route type of an NLRI it cannot decode
// (types 6-11 are registered with IANA and this decoder carries their bytes
// in Raw), so a value outside 1-5 can genuinely reach this column.
//
// Labels is the label stack as received -- type 2 may carry two -- and an
// empty array means no labels, with none of the null-versus-0 ambiguity
// VPNRoute's Label/HasLabel pair exists to resolve: a stack has a length,
// where a scalar label has only a value. Two things about its contents are
// worth knowing before comparing it to VPNRoute.Label. These are RAW 24-bit
// label fields, not shifted MPLS labels (see bgp.EvpnRoute.Labels: on a
// VXLAN fabric the value is the VNI), and consequently the RFC 3107 withdraw
// sentinel appears here as 0x800000 = 8388608, not as VPNRoute's shifted
// withdrawSentinel = 524288. Every archive row carrying 8388608 is also
// is_withdraw = 1, and evpnRoutesSQL's HAVING excludes all of them, so
// nothing here strips it -- the contract asks for the stack "as received",
// and a stripped value would be a decision this package has no encapsulation
// community to justify.
//
// GatewayIP is a plain string, not a netip.Addr, because the contract types
// it as a string and because "" is a real value 136 of the archive's 202
// rows carry (every type-2 and type-3 row). Parsing it into a netip.Addr
// would render those as "invalid IP" -- a self-describing failure for a
// field that never failed at anything. NextHop is a netip.Addr for the
// opposite reason: it is the field a looking glass compares against a
// Peer.PeerIP, and unmapAll's contract covers it.
//
// RouterIP, PeerIP and NextHop are in plain form, ASPath and OriginASN are
// the same two claims Route documents (every one of the archive's 202
// route_evpn rows has an empty AS path, so OriginASN is 0 for all of them
// and a caller must check len(ASPath) == 0 before reading it), RouteTargets
// is empty rather than an error on a route that has none (which is every
// archive row today), and Collector is the collector whose view this row is
// -- all four exactly as Route and VPNRoute document them.
//
// DumpState is the same field Route.DumpState documents, scoped to this
// route's own family, which for this table is always evpn -- route_evpn has
// no family column because an EVPN table holds exactly one. See evpnFamily
// for how that token is derived rather than spelled.
//
// There is deliberately no Family FIELD either, where Route and VPNRoute
// both have one: api/openapi.yaml leaves family out of EVPNRoute.required
// for the same reason route_evpn leaves the column out of the table, and a
// field filled in from evpnFamily would be this package asserting a fact
// about a row rather than reporting one -- the same objection RouteType's
// missing name mapping records.
//
// MED, LocalPref, Communities and LargeCommunities are the same four fields
// Route carries, with the same two nullable columns and the same Go-side
// community rendering; read Route's own doc comment for the decisions behind
// them. They are worth having here even though every one of the archive's
// 202 route_evpn rows leaves all four empty: RouteCommon.required lists all
// four and EVPNRoute is an allOf over it, and an EVPN route really can carry
// them -- RFC 7432's own route targets arrive as extended communities on the
// very same attribute, which this table already stores beside them.
type EVPNRoute struct {
	RouterSysName             string
	RouterIP, PeerIP, NextHop netip.Addr
	Collector                 string
	RIB                       string
	RouteType                 uint8
	RD, Prefix                string
	MAC, IP                   string
	GatewayIP                 string
	EthernetTag               uint32
	ESI                       string
	PathID                    uint32
	Labels                    []uint32
	RouteTargets              []string
	ASPath                    []uint32
	OriginASN                 uint32
	MED                       *uint32
	LocalPref                 *uint32
	Communities               []string
	LargeCommunities          []string
	// ExtCommunities is the same field and the same reason Route's own doc
	// comment gives; EVPNRoute already carries RouteTargets, the rt: subset
	// of it, because the statement already selects r.route_targets AS
	// live_route_targets.
	ExtCommunities []string
	DumpState      string
}
