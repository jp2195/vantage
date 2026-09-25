import type { LsLink, LsNode } from '@/api/generated'

/**
 * What a distinct adjacency IS, as one pure function, because two files have
 * to agree about it.
 *
 * `LinkStateGraph` draws these and the screen around it counts them, and
 * those two answers have to be the same answer. `@/lib/trimGraph` exists for
 * exactly this reason one screen over, and its doc comment records what it
 * cost to learn: `TopologyView`'s summary line and its empty-state gate read
 * the untrimmed graph while the canvas drew the trimmed one, so a threshold
 * above the heaviest edge left a blank pane with "5 ASNs · 4 edges" written
 * above it. The link-state screen has a wider version of the same gap --
 * 229 `ls_links` rows in the archive collapse to 20, 8 and 7 distinct
 * adjacencies across the three views that have any -- so the collapse is
 * shared before a second caller exists rather than after one has drifted.
 *
 * Rule 2 is the reason the collapse exists at all: node, adjacency and prefix
 * counts are counts of DISTINCT objects, and reporting a row count as a
 * topology count is a defect that is easy to make and hard to see.
 *
 * "Rule N" in this file means the link-state rule of that number, as written
 * out in LinkStateGraph.vue's doc comment.
 */
export interface Adjacency {
  /** `${local.node_key}-${remote.node_key}`, and the drawing's identity. */
  key: string
  /** The local end's node_key. */
  src: string
  /** The remote end's node_key. */
  dst: string
  /** Every distinct IGP metric among the rows collapsed here, ascending. */
  metrics: number[]
  /**
   * Every distinct max_bandwidth among them, ascending, raw and unconverted.
   * BGP-LS carries this in BYTES per second (RFC 5305 sec 3.4), so a caller
   * rendering it owes the reader the unit.
   */
  bandwidths: number[]
  /** How many distinct parallel links this adjacency stands for. */
  members: number
  /** The reverse direction is absent from the data (rule 5). */
  oneSided: boolean
}

/**
 * How a ROW identifies one physical link, minus the observer.
 *
 * `query.LSLink` keys a row by the observer plus both node keys, both
 * interface addresses and both link IDs, and its doc comment says why:
 * parallel links between one pair of nodes are ordinary, and "collapsing them
 * would report a redundant pair as a single adjacency: an operator looking at
 * a bundle would see one link where there are two, which is the difference
 * between 'this path has no redundancy' and 'it does'".
 *
 * Counting rows instead would be the same defect pointing the other way: a
 * row repeated -- by a re-read, by a caller that concatenated two pages, by a
 * second observer's copy of one link -- would invent redundancy that is not
 * in the data.
 *
 * The blind spot is the query's own and is stated rather than papered over:
 * two links that advertise neither interface address nor link ID produce the
 * identical tuple and count once. Nothing available here can tell them apart,
 * so a caller must report "one link as the database identifies links" rather
 * than "one link".
 */
function linkIdentity(link: LsLink): string {
  const { local, remote } = link
  return `${local.ifaddr}|${local.interface_id}|${remote.ifaddr}|${remote.interface_id}`
}

const ascending = (a: number, b: number) => a - b

/**
 * Every distinct directed adjacency in `links`.
 *
 * The node pair is the key. Two parallel links run between the same two
 * nodes, so a drawing keyed any more finely would put one line exactly on top
 * of another and report redundancy by making it invisible; what the collapse
 * merged is carried on the adjacency instead, in `metrics`, `bandwidths` and
 * `members`.
 *
 * `oneSided` is read against every key in `links`, before any caller drops
 * anything: an adjacency is two-sided when the reversed key is also present
 * in the DATA, whatever a caller then chooses to draw. It is symmetric by
 * construction -- the reverse of a pair names the same two nodes -- so a
 * filter over the node set cannot move it.
 *
 * Whether a one-sided adjacency is one-directional in the network or simply
 * outside this router's view is NOT decidable from this data, and nothing
 * here decides it (rule 5). In the surviving archive it tracks view
 * completeness: the router with the complete view has none of them and the
 * thinnest view has 5 of its 7.
 */
export function distinctAdjacencies(links: ReadonlyArray<LsLink>): Adjacency[] {
  const merged = new Map<
    string,
    { src: string; dst: string; metrics: Set<number>; bandwidths: Set<number>; links: Set<string> }
  >()
  for (const link of links) {
    const key = `${link.local.node_key}-${link.remote.node_key}`
    let held = merged.get(key)
    if (!held) {
      held = {
        src: link.local.node_key,
        dst: link.remote.node_key,
        metrics: new Set(),
        bandwidths: new Set(),
        links: new Set(),
      }
      merged.set(key, held)
    }
    held.metrics.add(link.igp_metric)
    held.bandwidths.add(link.max_bandwidth)
    held.links.add(linkIdentity(link))
  }
  return [...merged].map(([key, held]) => ({
    key,
    src: held.src,
    dst: held.dst,
    metrics: [...held.metrics].sort(ascending),
    bandwidths: [...held.bandwidths].sort(ascending),
    members: held.links.size,
    oneSided: !merged.has(`${held.dst}-${held.src}`),
  }))
}

/**
 * The adjacencies a caller holding `nodes` can actually draw: both ends in
 * the node set.
 *
 * A thin LSDB produces what this drops -- an adjacency naming a node whose
 * row this router never got. `forceLink` throws "node not found" on an edge
 * like that, and `settleGraph` drops one itself, but a caller must not lean
 * on that: the set laid out, the set drawn and the set COUNTED all have to be
 * one set, and a filter that lives only inside the layout leaves the count
 * free to disagree with the picture.
 *
 * The rejected alternative is promoting the endpoint into a node. An
 * `LsEndpoint` carries an asn, an area, a router-id and a label, so a node
 * could be synthesized from one -- and it would be a router this LSDB does
 * not hold, which is a screen inventing the fleet-wide topology rule 1 says
 * no single LSDB can describe.
 */
export function drawnAdjacencies(
  links: ReadonlyArray<LsLink>,
  nodes: ReadonlyArray<LsNode>,
): Adjacency[] {
  const held = new Set(nodes.map((n) => n.node_key))
  return distinctAdjacencies(links).filter((a) => held.has(a.src) && held.has(a.dst))
}

/**
 * The link ROWS behind the adjacencies `drawnAdjacencies` keeps.
 *
 * Separate from the adjacencies themselves because an adjacency is a
 * collapse and deliberately carries none of the row's scope fields -- and
 * rule 4 is a question about the scope: a caller naming the protocol and area
 * it drew has to read the rows that produced the lines, not the lines.
 */
export function drawnLinkRows(
  links: ReadonlyArray<LsLink>,
  nodes: ReadonlyArray<LsNode>,
): LsLink[] {
  const held = new Set(nodes.map((n) => n.node_key))
  return links.filter((l) => held.has(l.local.node_key) && held.has(l.remote.node_key))
}

/**
 * Every distinct NODE in `nodes`, keyed by `node_key`, first row seen wins.
 *
 * The node axis of the same rule the rest of this module carries, and it
 * lives here for the reason stated at the top: the collapse is shared before
 * a second caller exists rather than after one has drifted. `LinkStateGraph`
 * lays out one box per node it is given and the screen counts what the canvas
 * drew, so both have to mean the same thing by "a node".
 *
 * A row is one OBSERVATION of a node, not a node. `LSNodes` groups by
 * `(collector, router_ip, peer_ip, rib, protocol, identifier, asn, bgpls_id,
 * area, router_id, node_key)`, so one router monitored through two BMP peers
 * -- or reporting both a pre- and a post-policy RIB -- returns every node it
 * holds more than once. The archive's 139 `ls_nodes` rows are 7 distinct
 * nodes in the richest view.
 *
 * `node_key` is the whole identity and nothing narrower is needed: the column
 * is `cityHash64(protocol, identifier, asn, bgpls_id, area, router_id)`
 * (deploy/clickhouse/schema.sql:326), so two rows sharing a key already agree
 * about both halves of rule 4's scope. This collapse cannot merge two
 * topologies into one node; what it merges is one node reported twice.
 *
 * The rows it discards differ only in which observation they came from, and
 * "first seen" is the answer's own order rather than a choice made here --
 * the resolved per-node fields (`name`, the SR ranges, `is_withdraw`) are
 * already argMax'd server-side within each group, so the duplicates a caller
 * sees here disagree only where two PEERS of one router disagree, which the
 * API models as two rows on purpose.
 */
export function distinctNodes(nodes: ReadonlyArray<LsNode>): LsNode[] {
  const held = new Map<string, LsNode>()
  for (const n of nodes) if (!held.has(n.node_key)) held.set(n.node_key, n)
  return [...held.values()]
}
