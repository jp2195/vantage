package query

import (
	"fmt"
	"net/netip"
)

// LSState is which link-state objects an answer includes, as a type.
// The zero value is
// the empty string and means LSStateLive, so a caller that never sets the
// field gets the same default every other resource in this package applies:
// objects that are currently present.
//
// It is a named string rather than a bool or an int because all three values
// are reachable from a query parameter and have to survive the round trip
// with their own names -- api/openapi.yaml documents the enum, and the
// contract's frozen-enum test pins it.
type LSState string

const (
	LSStateLive      LSState = "live"
	LSStateWithdrawn LSState = "withdrawn"
	LSStateAny       LSState = "any"
)

// predicate renders the one post-aggregation predicate a state asks for, and
// refuses a state this package does not know rather than falling through to
// the default. The looser reading is silent in the worst way: a caller who
// typed "livee" would be told about live objects and have no way to discover
// the filter was ignored.
//
// Rendering and refusing are ONE call, deliberately. The first cut had them
// apart -- a having() reporting (expr, ok) beside a checkState() that
// consulted it -- and that arrangement is safe only for as long as every
// entry point remembers to run the check before it renders. Forgetting is
// not loud: an unrecognized state renders the empty string, and the empty
// string is exactly what LSStateAny renders, so the forgotten check does not
// produce an error or an empty answer. It produces state=any, silently, for
// a state nobody asked for -- the precise failure this refusal exists to
// prevent, reintroduced by the shape of the code that prevents it. Fused,
// a caller cannot hold a rendered predicate without having already proved
// the state was one of the three, and that is a property of the signature
// rather than of a convention each new resource has to remember.
//
// LSStateAny renders NOTHING, which is why the link-state statements end in
// havingClause rather than in a fixed HAVING: there is a legitimate query
// here with no post-aggregation predicate, and the route statements have no
// such case. It is also why the empty string cannot double as the error
// signal, and why the refusal is a separate return value rather than a
// sentinel expression.
func (s LSState) predicate() (string, error) {
	switch s {
	case "", LSStateLive:
		return "live_is_withdraw = 0", nil
	case LSStateWithdrawn:
		return "live_is_withdraw = 1", nil
	case LSStateAny:
		return "", nil
	default:
		return "", fmt.Errorf("%w: state %q is not one of live, withdrawn, any",
			ErrBadFilter, s)
	}
}

// LSNodeFilter is the narrowing LSNodes accepts: /v1/ls/nodes' documented
// parameters.
//
// Area, ASN and NodeKey are POINTERS, and that is not a style choice. Zero
// is a real, common value in every one of those columns -- area 0 is the
// OSPF backbone and 85% of the archive's ls_nodes rows, bgpls_id is 0 on
// 100% of them, and a node with no AS descriptor stores asn 0 -- so the
// "zero means not asked" convention RouteFilter uses would make the single
// most-asked-about area unaskable. See RouteFilter.Prefix's own doc
// comment for the same defect in its first form.
//
// Protocol is NOT a pointer, because Protocol-ID 0 is unassigned and
// ParseProtocol refuses it, so 0 is free to mean "not asked" there.
type LSNodeFilter struct {
	Router, Peer netip.Addr
	RIB          string

	Protocol uint8
	Area     *uint32
	ASN      *uint32
	NodeKey  *uint64

	State LSState
	Limit int

	// Cursor is set only by a paginated walk; predicates() ignores it,
	// LSNodesPage consumes it. It carries the position a previous page of
	// this same walk ended at, not a narrowing a caller asks for, so it has
	// no place in the WHERE/HAVING predicates() builds -- see RIBCursor and
	// LSNodesPage for what it pins and how it is validated.
	//
	// LSNodesPage refuses a request that sets BOTH Cursor and any of
	// Protocol, Area, ASN, NodeKey or a non-default State: RIBCursor has no
	// field to record which narrowing (if any) the walk that issued it was
	// running under, so nothing downstream can tell a continued walk from
	// one whose narrowing silently changed between pages. See narrowing.
	Cursor *RIBCursor
}

// narrowing names the first node-identity narrowing set on f, or "" if f
// asks for every node in scope. It exists for LSNodesPage's cursor
// compatibility check (see Cursor's own doc comment): a narrowing removes
// rows from what the walk's ORDER BY ever sorts at all -- Protocol, Area,
// ASN and NodeKey via the WHERE, State via the HAVING that runs after
// GROUP BY lsNodesKey aggregates -- so a walk that started under one and
// continued under another (or under none) would silently return rows past
// the cursor from the wrong scope, with nothing anywhere to catch it.
//
// State counts as a narrowing here even though its zero value ("") and
// LSStateLive both mean the same live-only default; either is "no
// narrowing", and only Withdrawn or Any moves rows across the HAVING boundary
// relative to that default.
func (f LSNodeFilter) narrowing() string {
	switch {
	case f.Protocol != 0:
		return "protocol"
	case f.Area != nil:
		return "area"
	case f.ASN != nil:
		return "asn"
	case f.NodeKey != nil:
		return "node_key"
	case f.State != "" && f.State != LSStateLive:
		return "state"
	default:
		return ""
	}
}

// Narrowing is narrowing, exported for callers outside this package. The
// api handlers need the same answer LSNodesPage's own cursor check uses --
// whether f narrows by node identity -- to decide whether a scoped, cursor-
// less request should be paginated at all: a narrowed page 1 could hand
// back a next_cursor the same filter can never legally accompany on the
// next request (see LSNodesPage's own doc comment). It is a one-line
// wrapper rather than a rename so that this package's own internal callers
// keep referring to the unexported name, unaffected by what the api
// package needs from it.
func (f LSNodeFilter) Narrowing() string { return f.narrowing() }

// predicates renders f against the alias lsNodesSQL uses, `n`. The alias is
// hard-coded for the reason RouteFilter.predicates hard-codes `r`: it is a
// property of that statement rather than of the caller, and a mismatch is a
// silently unfiltered query.
//
// Every predicate here is a WHERE except the state, which is a HAVING
// because is_withdraw is a mutable attribute: the newest observation decides
// whether an object is present, and a WHERE would remove rows before argMax
// picked among them, reporting a superseded observation as live. That is the
// same rule the wide filters follow, for the same reason, and it is proven
// directly against this archive.
//
// It returns an error rather than a bare *filters, and that signature is the
// whole of this filter's validation: there is no separate check() to call
// first, because a separate check() is a step an entry point can skip. See
// LSState.predicate for what skipping it used to cost. The state is resolved
// at the top, before any other predicate is built, so a rejected filter
// never renders a single placeholder.
func (f LSNodeFilter) predicates() (*filters, error) {
	state, err := f.State.predicate()
	if err != nil {
		return nil, err
	}
	var fl filters
	fl.eqAddr("n.router_ip", f.Router)
	fl.eqAddr("n.peer_ip", f.Peer)
	fl.eqNonEmpty("n.rib", f.RIB)
	if f.Protocol != 0 {
		fl.eq("n.protocol", f.Protocol)
	}
	if f.Area != nil {
		fl.eq("n.area", *f.Area)
	}
	if f.ASN != nil {
		fl.eq("n.asn", *f.ASN)
	}
	if f.NodeKey != nil {
		fl.eq("n.node_key", *f.NodeKey)
	}
	if state != "" {
		fl.havingExpr(state)
	}
	return &fl, nil
}

// LSPrefixFilter is LSNodeFilter plus the two prefix-space parameters
// /v1/ls/prefixes documents. Area, ASN and NodeKey are pointers for section
// 3.0's reason -- see LSNodeFilter.
//
// Prefix is OPTIONAL and "" means "every prefix", which is
// VPNRouteFilter.Prefix's reading rather than RouteFilter.Prefix's. The two
// differ and the difference has bitten this repo three times, so it is
// written out here rather than inherited: there is no /v1/ls/prefixes
// question that means "the empty prefix", because a prefix is assembled from
// two columns and the empty one is not a value either can hold.
type LSPrefixFilter struct {
	Router, Peer netip.Addr
	RIB          string

	Protocol uint8
	Area     *uint32
	ASN      *uint32
	NodeKey  *uint64

	Prefix string
	Covers string

	State LSState
	Limit int

	// Cursor is set only by a paginated walk; predicates() ignores it,
	// LSPrefixesPage consumes it. It carries the position a previous page of
	// this same walk ended at, not a narrowing a caller asks for, so it has
	// no place in the WHERE/HAVING predicates() builds -- see RIBCursor and
	// LSPrefixesPage for what it pins and how it is validated.
	//
	// LSPrefixesPage refuses a request that sets BOTH Cursor and any of
	// Protocol, Area, ASN, NodeKey, Prefix, Covers or a non-default State:
	// RIBCursor has no field to record which narrowing (if any) the walk
	// that issued it was running under, so nothing downstream can tell a
	// continued walk from one whose narrowing silently changed between
	// pages. See narrowing.
	Cursor *RIBCursor
}

// narrowing names the first narrowing set on f, or "" if f asks for every
// prefix in scope. It exists for LSPrefixesPage's cursor compatibility check
// (see Cursor's own doc comment), and it is LSNodeFilter.narrowing's
// argument carried over to a wider filter: Protocol, Area, ASN and NodeKey
// narrow by the node that originates a prefix, exactly as they narrow node
// identity there. Prefix and Covers are the two fields LSNodeFilter has no
// counterpart for -- they narrow the prefix itself, via the WHERE, the same
// way the identity fields do -- and State narrows via the HAVING that runs
// after GROUP BY lsPrefixesKey's remainder aggregates. Any of the seven
// removes rows from what the walk's ORDER BY ever sorts at all, so a walk
// that started under one and continued under another (or under none) would
// silently return rows past the cursor from the wrong scope, with nothing
// anywhere to catch it.
//
// State counts as a narrowing here even though its zero value ("") and
// LSStateLive both mean the same live-only default; either is "no
// narrowing", and only Withdrawn or Any moves rows across the HAVING
// boundary relative to that default.
func (f LSPrefixFilter) narrowing() string {
	switch {
	case f.Protocol != 0:
		return "protocol"
	case f.Area != nil:
		return "area"
	case f.ASN != nil:
		return "asn"
	case f.NodeKey != nil:
		return "node_key"
	case f.Prefix != "":
		return "prefix"
	case f.Covers != "":
		return "covers"
	case f.State != "" && f.State != LSStateLive:
		return "state"
	default:
		return ""
	}
}

// Narrowing is narrowing, exported for callers outside this package -- see
// LSNodeFilter.Narrowing's own doc comment for why it exists and why it is
// a wrapper rather than a rename.
func (f LSPrefixFilter) Narrowing() string { return f.narrowing() }

// predicates renders f against lsPrefixesSQL's alias, `p`, and is this
// filter's ENTIRE validation as well as its rendering -- there is no
// separate check() to call first. That is LSNodeFilter.predicates' rule
// applied a second time: a validation step an entry point can forget to run
// is a validation step some entry point eventually will, and LSState.predicate
// exists precisely because that happened once already in this package. See
// its own doc comment.
//
// Two checks run before a single predicate is built, so a rejected filter
// never renders a placeholder: checkCovers refuses a Prefix and a Covers
// asked for together, the same rule and the same helper the three route
// filters already share -- api/openapi.yaml documents the identical pair on
// /v1/ls/prefixes, and the mistake means the same thing here it means there,
// an exact match that also demands containment, whose empty result reads
// like the prefix being nowhere in the fleet. LSState.predicate refuses a
// state this package does not recognize, for lsNodesSQL's own reason.
func (f LSPrefixFilter) predicates() (*filters, error) {
	if err := checkCovers("/v1/ls/prefixes", f.Prefix, f.Covers); err != nil {
		return nil, err
	}
	state, err := f.State.predicate()
	if err != nil {
		return nil, err
	}
	var fl filters
	fl.eqAddr("p.router_ip", f.Router)
	fl.eqAddr("p.peer_ip", f.Peer)
	fl.eqNonEmpty("p.rib", f.RIB)
	if f.Protocol != 0 {
		fl.eq("p.protocol", f.Protocol)
	}
	if f.Area != nil {
		fl.eq("p.area", *f.Area)
	}
	if f.ASN != nil {
		fl.eq("p.asn", *f.ASN)
	}
	if f.NodeKey != nil {
		fl.eq("p.node_key", *f.NodeKey)
	}
	// At most one prefix-space predicate, the rule RouteFilter.predicates
	// keeps: an exact match left in alongside a containment test would ask
	// for a prefix that also contains an address and answer nothing at all.
	// checkCovers above has already refused the pair, so this is the second
	// guard rather than the only one -- see coversIfAsked's own doc comment
	// for why the parse error it drops is safe here too: an unparseable
	// Covers already failed checkCovers and never reaches this line.
	if !fl.coversIfAsked(lsPrefixCIDR, f.Covers) && f.Prefix != "" {
		fl.eq(lsPrefixCIDR, f.Prefix)
	}
	if state != "" {
		fl.havingExpr(state)
	}
	return &fl, nil
}

// LSLinkFilter is the narrowing LSLinks accepts: /v1/ls/links' documented
// parameters.
//
// Area and ASN match EITHER END, which is a departure from LSNodeFilter and
// LSPrefixFilter and is forced by the data rather than chosen: ls_links has
// no plain area or asn column at all, only local_/remote_ pairs, because a
// link describes two nodes and can cross between them. An operator asking
// for "the links in area 0" means the links TOUCHING area 0, and the
// either-end reading is the only one that also returns the area-crossing
// links -- the ones most worth seeing, and the ones a both-ends reading
// would silently drop.
//
// Area, ASN, LocalNode and RemoteNode are pointers for the reason
// LSNodeFilter's own doc comment gives: zero is a real and common value in
// every one of those columns, so the "zero means not asked" convention
// RouteFilter uses would make the most-asked-about area unaskable. Protocol
// is not a pointer, because Protocol-ID 0 is unassigned.
//
// LocalNode and RemoteNode are the exact form for a caller who needs one
// specific end, and they are deliberately NOT either-end: a caller naming a
// node_key is naming a direction, and folding the two into one predicate
// would make "the links out of this node" and "the links into it" the same
// question.
type LSLinkFilter struct {
	Router, Peer netip.Addr
	RIB          string

	Protocol   uint8
	Area       *uint32
	ASN        *uint32
	LocalNode  *uint64
	RemoteNode *uint64

	State LSState
	Limit int

	// Cursor is set only by a paginated walk; predicates() ignores it,
	// LSLinksPage consumes it. It carries the position a previous page of
	// this same walk ended at, not a narrowing a caller asks for, so it has
	// no place in the WHERE/HAVING predicates() builds -- see RIBCursor and
	// LSLinksPage for what it pins and how it is validated.
	//
	// LSLinksPage refuses a request that sets BOTH Cursor and any of
	// Protocol, Area, ASN, LocalNode, RemoteNode or a non-default State:
	// RIBCursor has no field to record which narrowing (if any) the walk
	// that issued it was running under, so nothing downstream can tell a
	// continued walk from one whose narrowing silently changed between
	// pages. See narrowing.
	Cursor *RIBCursor
}

// narrowing names the first narrowing set on f, or "" if f asks for every
// link in scope. It exists for LSLinksPage's cursor compatibility check
// (see Cursor's own doc comment), and it is LSNodeFilter.narrowing's
// argument carried over to this filter's own field list: Protocol narrows
// by the routing protocol the link was learned from, Area and ASN narrow by
// EITHER end (see this filter's own doc comment on why), LocalNode and
// RemoteNode narrow by one specific end, and State narrows via the HAVING
// that runs after GROUP BY lsLinksKey's remainder aggregates. Any of the
// six removes rows from what the walk's ORDER BY ever sorts at all, so a
// walk that started under one and continued under another (or under none)
// would silently return rows past the cursor from the wrong scope, with
// nothing anywhere to catch it.
//
// State counts as a narrowing here even though its zero value ("") and
// LSStateLive both mean the same live-only default; either is "no
// narrowing", and only Withdrawn or Any moves rows across the HAVING
// boundary relative to that default.
func (f LSLinkFilter) narrowing() string {
	switch {
	case f.Protocol != 0:
		return "protocol"
	case f.Area != nil:
		return "area"
	case f.ASN != nil:
		return "asn"
	case f.LocalNode != nil:
		return "local_node"
	case f.RemoteNode != nil:
		return "remote_node"
	case f.State != "" && f.State != LSStateLive:
		return "state"
	default:
		return ""
	}
}

// Narrowing is narrowing, exported for callers outside this package -- see
// LSNodeFilter.Narrowing's own doc comment for why it exists and why it is
// a wrapper rather than a rename.
func (f LSLinkFilter) Narrowing() string { return f.narrowing() }

// predicates renders f against lsLinksSQL's alias, `l`, and -- like
// LSNodeFilter.predicates and LSPrefixFilter.predicates -- is this filter's
// ENTIRE validation as well as its rendering. There is no check() sibling to
// call first, deliberately: a validation step an entry point can forget to
// run is one some entry point eventually will, and LSState.predicate exists
// precisely because that happened once already in this package. See its own
// doc comment for what the split version cost.
//
// The state is resolved before any other predicate is built, so a rejected
// filter never renders a single placeholder.
//
// local_node_key and remote_node_key are MATERIALIZED columns rather than
// stored ones (see schema.sql), which changes nothing here -- ClickHouse
// evaluates them for a WHERE the same way it does for a SELECT -- but is
// worth knowing when reading the statement beside this: they are cityHash64
// over the six node-identity columns, so a caller filtering on one is
// filtering on the same identity ls_nodes' own node_key carries, which is
// what makes LSNode.NodeKey a value a caller can hand straight back here.
func (f LSLinkFilter) predicates() (*filters, error) {
	state, err := f.State.predicate()
	if err != nil {
		return nil, err
	}
	var fl filters
	fl.eqAddr("l.router_ip", f.Router)
	fl.eqAddr("l.peer_ip", f.Peer)
	fl.eqNonEmpty("l.rib", f.RIB)
	if f.Protocol != 0 {
		fl.eq("l.protocol", f.Protocol)
	}
	// Either end, rendered as ONE parenthesized OR rather than as two
	// predicates: two would be ANDed by where(), which asks for a link
	// carrying that area at BOTH ends and silently excludes every
	// area-crossing link -- the opposite of what this filter is for. See
	// filters.orEq for why the parentheses it writes are load-bearing.
	if f.Area != nil {
		fl.orEq("l.local_area", "l.remote_area", *f.Area)
	}
	if f.ASN != nil {
		fl.orEq("l.local_asn", "l.remote_asn", *f.ASN)
	}
	if f.LocalNode != nil {
		fl.eq("l.local_node_key", *f.LocalNode)
	}
	if f.RemoteNode != nil {
		fl.eq("l.remote_node_key", *f.RemoteNode)
	}
	if state != "" {
		fl.havingExpr(state)
	}
	return &fl, nil
}
