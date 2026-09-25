package query

import "reflect"

// lsRibColumn is ribColumn for the link-state statements, which alias their
// table (n, p, l) where the route statements use r. The domain matters: rib is
// an Enum8, and a cursor value outside the enum raises rather than comparing
// false, which turns a mangled cursor into a 500 for a failure the contract
// types as a 400. See ribKeyCol.domain.
func lsRibColumn(alias string) ribKeyCol {
	return ribKeyCol{col: alias + ".rib", want: reflect.TypeFor[string](), domain: ribMembers}
}

// lsNodesKey is the scoped walk's ordering: identity only. live_name is
// deliberately absent -- it is argMax(name, ...) and advances within a pinned
// session, so ordering on it lets a row move between pages. protocol,
// identifier, asn, bgpls_id and area are absent too: node_key is
// MATERIALIZED as cityHash64(protocol, identifier, asn, bgpls_id, area,
// router_id), so router_id and node_key together already stand in for all
// five of GROUP BY's own remaining identity columns, up to a hash collision.
var lsNodesKey = []ribKeyCol{
	lsRibColumn("n"),
	{col: "n.router_id", want: reflect.TypeFor[string]()},
	{col: "n.node_key", want: reflect.TypeFor[uint64]()},
}

// lsNodesPageOrder is lsNodesOrder's counterpart for a paginated walk: ORDER
// BY lsNodesKey rather than lsNodesOrder's five-column presentational lead.
//
// It is DERIVED from lsNodesKey via keyOrder (see that function's own doc
// comment in rib.go) rather than written out by hand beside it, which is the
// one property this package cannot afford to lose here: TestRIBOrderAnd-
// KeysetNameTheSameColumns holds keyOrder's output to (*filters).keyset's
// predicate for lsNodesKey the same way it does for all three RIB families,
// so the two cannot drift the way a hand-written ORDER BY a few lines away
// from its key could.
//
// It carries no LIMIT, matching the convention LSNodes' own call site sets --
// fmt.Sprintf(lsNodesSQL+lsNodesPageOrder, ...) and then the caller appends
// `LIMIT %d` itself -- because the limit here is the clamped page size plus
// one probe row, a value LSNodes never has to compute. That is also why this
// is keyOrder(lsNodesKey) rather than ribOrder(lsNodesKey): ribOrder appends
// a bound `LIMIT ?`, which is right for the three RIB statements and wrong
// here.
var lsNodesPageOrder = keyOrder(lsNodesKey)

// lsPrefixesKey is the scoped prefix walk's ordering: identity only, the
// same shape as lsNodesKey. Prefixes are the easy family, unlike nodes and
// links: cidr -- lsPrefixCIDR, concat(p.prefix, '/', toString(p.prefix_len))
// -- is derived from the two immutable storage columns rather than
// aggregated the way live_name and the two link labels are, so the lead
// column a caller would actually want to read by is ALSO a safe keyset
// column, and there is no need to drop it from the key the way lsNodesKey
// drops live_name.
//
// p.prefix and p.prefix_len are themselves absent from this key: cidr is
// exactly concat(p.prefix, '/', toString(p.prefix_len)), so it already
// stands in for both of GROUP BY's storage columns without repeating either.
//
// p.node_key is still listed after cidr, and it is not redundant the way
// n.node_key is in lsNodesKey: cidr is injective over (prefix, prefix_len)
// but not over the whole row, because lsPrefixesSQL's GROUP BY carries
// node_key alongside (prefix, prefix_len) -- the same prefix genuinely can
// be originated by two different nodes (an anycast address is the ordinary
// case), and that is two rows sharing one cidr rather than one. node_key
// breaks the tie between them; see insertLSPrefixPageFixture's own doc
// comment for a fixture built to exercise exactly that.
var lsPrefixesKey = []ribKeyCol{
	lsRibColumn("p"),
	{col: lsPrefixCIDR, want: reflect.TypeFor[string]()},
	{col: "p.node_key", want: reflect.TypeFor[uint64]()},
}

// lsPrefixesPageOrder is lsPrefixesOrder's counterpart for a paginated walk,
// derived from lsPrefixesKey via keyOrder for lsNodesPageOrder's own reason:
// TestRIBOrderAndKeysetNameTheSameColumns holds keyOrder's output to
// (*filters).keyset's predicate for lsPrefixesKey the same way it does for
// lsNodesKey and the three RIB families, so the two cannot drift the way a
// hand-written ORDER BY a few lines away from its key could.
var lsPrefixesPageOrder = keyOrder(lsPrefixesKey)

// lsLinksKey is the scoped link walk's ordering, and the widest of the
// three: identity only, exactly as lsNodesKey and lsPrefixesKey are, but
// links are the family where "identity only" excludes the most. local_label
// and remote_label are deliberately absent -- like live_name in lsNodesKey,
// they are any() over ls_node_labels / ls_fleet_labels (see
// lsNodeLabelsCTE), themselves derived from node names, so they move when a
// name changes without the link itself changing at all. Ordering a walk on
// either would let a row move between pages out from under its own cursor.
//
// The two interface addresses are NOT optional, unlike everything past rib
// in lsNodesKey and lsPrefixesKey. Parallel links between the same pair of
// nodes are ordinary -- see LSLink's own doc comment -- and measured
// 2026-09-02: up to 2 distinct (local_ifaddr, remote_ifaddr) pairs share one
// (local_node_key, remote_node_key, link_local_id, link_remote_id) in the
// live archive. A key ending at link_remote_id ties every such pair, and a
// keyset walk that straddles the tie at a page boundary drops one of the two
// and keeps the other -- the defect TestLSLinksPageWalksParallelLinksExactly-
// Once exists to catch, and does: deleting local_ifaddr and remote_ifaddr
// from this key (and so from lsLinksPageOrder, since that is derived from
// this slice) makes it fail.
//
// l.local_router_id leads the node-identity columns, matching lsNodesKey's
// own lead: it is not by itself sufficient to break every tie (two
// observers can share a router_id under different protocol/ASN/area
// combinations, which is exactly what local_node_key's hash also covers),
// but it is listed for lsNodesOrder's stated reason -- it is part of what a
// caller reads and part of what a later edit trimming a hash input would
// otherwise find silently un-covered here. local_node_key and
// remote_node_key are the columns actually doing the identity work past
// rib: each is cityHash64 over protocol, identifier, its own ASN/BGP-LS-
// ID/area and its own router_id (see schema.sql), so the pair stands in for
// all twelve of those columns without repeating any of them in the key.
//
// This is a physical column list, not an expression, because of a drift
// found in review: lsPrefixesKey's cidr column
// is the lsPrefixCIDR expression, which spells the sort key one way in the
// keyset predicate and another way (the `cidr` SELECT alias) in
// lsPrefixesOrder's unpaginated statement. That drift is harmless where it
// sits, because lsPrefixesOrder never has to agree with lsPrefixesPageOrder
// -- they are two different orderings for two different call sites -- but it
// is a wrinkle worth not repeating here on purpose. Nothing links needs is
// an expression: local_router_id, local_node_key, remote_node_key,
// link_local_id, link_remote_id, local_ifaddr and remote_ifaddr are all
// physical (or, for the two node keys, MATERIALIZED-but-column-shaped)
// columns on ls_links itself, so lsLinksKey spells them exactly as
// lsLinksSQL's own SELECT and GROUP BY do.
var lsLinksKey = []ribKeyCol{
	lsRibColumn("l"),
	{col: "l.local_router_id", want: reflect.TypeFor[string]()},
	{col: "l.local_node_key", want: reflect.TypeFor[uint64]()},
	{col: "l.remote_node_key", want: reflect.TypeFor[uint64]()},
	{col: "l.link_local_id", want: reflect.TypeFor[uint32]()},
	{col: "l.link_remote_id", want: reflect.TypeFor[uint32]()},
	{col: "l.local_ifaddr", want: reflect.TypeFor[string]()},
	{col: "l.remote_ifaddr", want: reflect.TypeFor[string]()},
}

// lsLinksPageOrder is lsLinksOrder's counterpart for a paginated walk,
// derived from lsLinksKey via keyOrder for lsNodesPageOrder's own reason:
// TestRIBOrderAndKeysetNameTheSameColumns holds keyOrder's output to
// (*filters).keyset's predicate for lsLinksKey the same way it does for
// lsNodesKey, lsPrefixesKey and the three RIB families, so the two cannot
// drift the way a hand-written ORDER BY a few lines away from its key
// could.
var lsLinksPageOrder = keyOrder(lsLinksKey)
