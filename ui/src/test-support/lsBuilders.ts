import type { LsCommon, LsEndpoint, LsLink, LsNode, LsPrefix } from '@/api/generated'

/**
 * Synthesized link-state rows, typed by the generated types.
 *
 * These are NOT captured fixtures, and nothing here may be read as a claim
 * about a response. `ui/src/api/fixtures/README.md` is the rule -- a fixture
 * comes off a live daemon -- and the link-state capture is currently blocked
 * on restoring `vantage_archive_backup` into `vantage`, because
 * `vantage.ls_*` holds no rows and neither FRR nor `bmpgen` can originate
 * BGP-LS. So the only claims these objects support are about a COMPONENT
 * handed rows of this shape. Key presence, `[]` versus `null`, the envelope
 * and `meta` are claims about the wire, and they belong to the captured
 * fixture, not here.
 *
 * What they do buy, and the reason they are typed rather than loose object
 * literals: `vue-tsc` checks them against the same generated types a real
 * response decodes into, so a contract change that renamed or dropped a
 * field fails the build here rather than drifting silently. A field this
 * file forgets is a compile error, not a runtime surprise -- which is how
 * `router_id_v4` came to be set below; every hand-written version of this
 * builder had omitted it.
 *
 * Shared rather than inlined per test file because three tasks need them --
 * the graph, the screen and the captured-fixture pass -- and two copies of a
 * builder drift the moment one of them is taught a new default.
 */

/**
 * The reporting router, its BMP peer and its session, identical on every row
 * a builder makes.
 *
 * One observer by default is the point rather than laziness: the screen draws
 * ONE router's view (LinkStateGraph.vue's rule 1), so rows that disagree about
 * `router_sysname` are a scope defect, and a test that wants that case has to
 * ask for it by overriding this.
 */
const OBSERVER = {
  router_sysname: 'nx-p3',
  router_ip: '10.0.0.11',
  peer_ip: '10.0.0.21',
  collector: 'dev-c1',
  rib: 'in_pre',
  protocol: 3,
  protocol_name: 'ospfv2',
  identifier: '100',
} as const

/**
 * The fields `LsNode` and `LsPrefix` share. `LsLink` does NOT extend
 * `LsCommon` -- an adjacency's identity lives in its two endpoints, not on
 * the row -- so `link()` below spells out its own.
 */
function common(key: string, routerId: string): LsCommon {
  return {
    ...OBSERVER,
    asn: 65000,
    bgpls_id: 0,
    area: 0,
    router_id: routerId,
    node_key: key,
    is_withdraw: false,
    dump_state: 'complete',
  }
}

/**
 * One node in the LSDB. `name` is empty by default; override it to name one.
 *
 * `router_id` and `router_id_v4` are set to the same dotted quad, and real
 * rows often disagree: `router_id` is the RAW identifier (hex -- see
 * `api/types_test.go`'s `0a0000f1`, and the contract's "hex for IS-IS system
 * IDs"), while `router_id_v4` is the dotted quad or "" when absent. A screen
 * must not read the two as interchangeable on the strength of these
 * builders; override them where that distinction is what a test is about.
 */
export function node(key: string, routerId: string, over: Partial<LsNode> = {}): LsNode {
  return {
    ...common(key, routerId),
    router_id_v4: routerId,
    name: '',
    srgb_base: 16000,
    srgb_size: 8000,
    srlb_base: 15000,
    srlb_size: 1000,
    sr_algorithms: [0],
    ...over,
  }
}

/**
 * One end of a link.
 *
 * `label` defaults to the router-id and `label_source` to `observer`, which
 * is the common case and also the STRONGEST of the three tiers -- so a test
 * about a borrowed name has to say `fleet` out loud rather than get it by
 * forgetting an argument.
 */
export function endpoint(
  key: string,
  routerId: string,
  label = routerId,
  source: LsEndpoint['label_source'] = 'observer',
): LsEndpoint {
  return {
    asn: 65000,
    bgpls_id: 0,
    area: 0,
    router_id: routerId,
    node_key: key,
    ifaddr: '10.1.1.1',
    interface_id: 0,
    label,
    label_source: source,
  }
}

/**
 * One directed adjacency row, from node `a` to node `b`.
 *
 * Both ends default to the same `ifaddr` and `interface_id`, so two rows
 * over the same node pair are the SAME link twice unless a caller overrides
 * them. That is the conservative default: `query.LSLink` keys a row by the
 * observer plus both node keys, both interface addresses and both link IDs
 * precisely because parallel links between one pair of nodes are ordinary,
 * and a builder that made every duplicate row look like a second physical
 * link would hand a test a redundancy that is not in the data.
 */
export function link(a: string, b: string, over: Partial<LsLink> = {}): LsLink {
  return {
    ...OBSERVER,
    local: endpoint(a, `10.255.0.${a}`),
    remote: endpoint(b, `10.255.0.${b}`),
    adj_sids: [],
    te_metric: 0,
    igp_metric: 10,
    admin_group: 0,
    max_bandwidth: 1e9,
    is_withdraw: false,
    dump_state: 'complete',
    ...over,
  }
}

/**
 * One prefix originated by node `key`.
 *
 * `prefix_sid_flags` defaults to 0, which is the flag set that makes
 * `prefix_sid` readable as an SRGB index: RFC 9085's V and L flags turn it
 * into an absolute label instead, and the contract says in as many words to
 * read the flags before the SID. 16001 is `node()`'s own `srgb_base` plus
 * one, so the default row is internally consistent rather than two unrelated
 * numbers.
 */
export function prefix(
  cidr: string,
  key: string,
  routerId: string,
  over: Partial<LsPrefix> = {},
): LsPrefix {
  return {
    ...common(key, routerId),
    prefix: cidr,
    prefix_sid: 16001,
    prefix_sid_flags: 0,
    has_prefix_sid: true,
    prefix_metric: 10,
    ospf_route_type: 1,
    ...over,
  }
}
