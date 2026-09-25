package query

import "fmt"

// PseudonodeSQL renders the predicate that identifies a LAN pseudonode from
// the columns already on ls_nodes and ls_links, so no schema change is needed
// to tell one from a router.
//
// A pseudonode is the link-state representation of a broadcast segment, not a
// device: the IGP elects a DR/DIS and models the LAN as a vertex every
// attached router adjoins, so what looks like an extra node in the graph is
// the Ethernet itself. Both IGPs signal it by widening the IGP Router-ID
// (RFC 9552 section 5.2.1.4) rather than by any flag:
//
//	IS-IS  6 bytes (system ID)                 -> router
//	IS-IS  7 bytes (system ID + PSN)           -> pseudonode
//	OSPF   4 bytes (Router-ID)                 -> router
//	OSPF   8 bytes (Router-ID + DR interface)  -> pseudonode
//
// router_id is stored hex-encoded (sink/rows.go), so the widths above are
// doubled here. The protocol has to be tested alongside the width because the
// two IGPs disagree about what each width means -- 8 hex characters is an
// OSPF router and 8 raw bytes is an OSPF pseudonode, and an unqualified
// length test would classify by coincidence.
//
// This is deliberately NOT a filter. Every one of the archive's 24 ospfv3
// links and all 124 of its isis-l1 links has a pseudonode at one end, because
// that is what adjoining a LAN means, so dropping pseudonodes from ls_nodes
// while keeping ls_links strands those edges on an endpoint that no longer
// exists. OpenBMP excludes the IS-IS form outright; that is safe only for a
// consumer that discards the links too. Callers that count devices should
// exclude pseudonodes; callers that draw or traverse the graph must keep
// them.
//
// If this ever earns a typed column it belongs beside node_key as a
// MATERIALIZED expression over these same inputs -- but schema.sql creates and
// never migrates, so a column costs a database recreation and this predicate
// costs nothing.
func PseudonodeSQL(protocolCol, routerIDCol string) string {
	return fmt.Sprintf(
		"((%[1]s IN (1, 2) AND length(%[2]s) = 14) OR (%[1]s IN (3, 6) AND length(%[2]s) = 16))",
		protocolCol, routerIDCol)
}
