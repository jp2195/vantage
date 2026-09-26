-- Vantage sink schema.
--
-- Every history table is append-only and carries the envelope columns.
-- Rows are per-route, exploded from per-UPDATE envelopes. The current-state
-- tables at the end of this file are fed from them; see their own header.
--
-- ReplacingMergeTree collapses rows sharing a sort tuple, which is what makes
-- JetStream's at-least-once redelivery safe: a redelivered envelope produces
-- byte-identical rows, which collapse back to one at merge time.
--
-- The corollary is the constraint every route table's ORDER BY below has to
-- satisfy, and it is stronger than it looks: **the sort key must contain
-- whatever distinguishes two routes carried by the same UPDATE**, or a merge
-- silently keeps one of them and discards the rest. It is not enough for
-- `prefix` to be in the key. `prefix` is not what distinguishes routes in two
-- of the three route tables:
--
--   * EVPN type-2 (MAC/IP) and type-3 (IMET) routes have no prefix at all --
--     `prefix` is '' for both -- and type-2 routes for one VLAN share RD, RT,
--     ESI, next hop and VNI label, differing only in MAC and IP. A leaf
--     advertising 200 MACs sends them in one UPDATE by construction.
--   * VPN routes repeat the same prefix under different RDs: two VRFs
--     exporting 10.9.9.0/24 as RD 65000:1 and RD 65000:2 is ordinary.
--   * Unicast routes repeat the same prefix under different add-path path-ids.
--
-- So each route table's sort key ends with that table's full route identity:
-- prefix + path_id + is_withdraw for unicast, + rd for VPN, and the NLRI
-- fields (route_type, rd, mac, ip, ethernet_tag, esi) for EVPN. Adding a
-- route-identifying column to a route table means adding it here too.
--
-- sink's TestRowsForDistinctSortTuplesPerRow enforces this from the
-- Go side: it reads the ORDER BY clauses out of this file and asserts that
-- one envelope's rows land on as many distinct sort tuples as there are rows.
-- TestClickHouseExplodedUpdateSurvivesMerge proves the same thing against a
-- real OPTIMIZE ... FINAL.
--
-- PARTITION BY and TTL are keyed on ts_collector, never ts_router.
-- ts_router comes from the BMP per-peer header and is not trustworthy: a
-- router with a zero clock (see collector/session.go's QK_TS_ZERO
-- quirk, which an operator can disable) writes 1970-01-01, landing rows in
-- partition 197001 already past the TTL, so the next merge deletes them.
-- A merely wrong non-zero clock is worse: future dates spray toYYYYMM
-- partitions across a 136-year range (~1600 of them), pushing the table
-- toward "too many parts" and failing inserts for every router at once.
-- ts_collector is the collector's own clock and monotonic-ish by
-- construction. ts_router stays as data, which is what it is.

CREATE DATABASE IF NOT EXISTS vantage;

-- schema_version exists so the writer can refuse to start against a shape it
-- does not expect, rather than inserting into the wrong columns quietly.
--
-- The version is 2. Version 1 is the squashed public baseline: every earlier
-- version step (peer_events' 'view_lost' kind, the column codecs, the peer
-- session facts) is folded into the CREATE statements below, and the
-- migrations that carried them were removed with it, since no archive outside
-- development predates them. Version 2 adds collector_beats, the collector
-- heartbeat table at the end of this file; migrations/001-collector-beats.sql
-- moves a version 1 database to it.
--
-- The codecs are not a guess. Per-column measurement on 10M rows put the cost
-- at prefix 4.44 B/row, ts_collector 4.44, stream_seq 4.14 and seq 3.91 --
-- together 96% of a 17.6 B/row row -- while every identity column an operator
-- would think to normalize away (router_ip, peer_ip, session_id, next_hop)
-- summed to 0.217 B/row, because they lead the sort key and compress to
-- almost nothing already. seq, stream_seq and the two timestamps are
-- monotonic within a session, which is what DoubleDelta is for, and they were
-- being stored with ClickHouse's default LZ4 and no delta stage at all.
--
-- Measured A/B through the real write path, same load, same everything else:
-- 4.11 B/row against 17.71 (4.3x smaller) at 98,395 rows/s against 98,884
-- (0.5%, inside run-to-run noise). The writer is not CPU-bound -- half a core
-- at saturation -- so ZSTD's extra compression cost is absorbed. Measured
-- 2026-09-04; see docs/measurements.md, "Collector and writer load test".
--
-- The header's version and the INSERT below must agree, and both must equal
-- sink.ExpectedSchemaVersion; TestExpectedSchemaVersion and
-- TestSchemaAndItsHighestMigrationDeclareTheSameVersion fail if any of them
-- moves alone.
--
-- This file CREATES; it never MIGRATES. Every statement here is IF NOT
-- EXISTS, and ClickHouse will not rewrite an existing table's ORDER BY, so
-- applying this to a database created by an older version leaves that
-- version's tables exactly as they were. The version row is therefore
-- inserted only into a database that has none -- because a version row is a
-- claim about the tables that are actually there, and applying this file to
-- a database built by an earlier shape does not make that claim true. Left
-- unconditional, it would stamp the current version over the old tables and
-- the writer's startup check would report success while inserts landed in
-- the wrong columns. That guard is one line of SQL that reads like
-- boilerplate; TestSchemaVersionInsertIsConditional is what stops it being
-- tidied away.
--
-- Moving an existing database forward is the job of
-- deploy/clickhouse/migrations/, which holds one file per version step after
-- the version 1 baseline (001 takes 1 to 2, and so on), each
-- carrying its change and its own conditional version row. Apply the ones
-- the database needs, in lexical order, after this file; the Helm chart's
-- schema Job does exactly that on every install and upgrade.
-- docs/deploying.md covers applying them by hand. Until they have run, a
-- database built by an older shape keeps its own version and the writer and
-- the API refuse to start against it, which is the outcome to want.
--
-- The dev stack mounts this directory as /docker-entrypoint-initdb.d, which
-- runs this file only on an empty volume and never runs the migrations; there
-- the simplest path to a new version is recreating the database with
-- `docker compose -f docker-compose.dev.yml down -v`.
CREATE TABLE IF NOT EXISTS vantage.schema_version
(
    version     UInt32,
    applied_at  DateTime DEFAULT now()
)
ENGINE = MergeTree ORDER BY version;

INSERT INTO vantage.schema_version (version)
SELECT 2 WHERE (SELECT count() FROM vantage.schema_version) = 0;

CREATE TABLE IF NOT EXISTS vantage.route_unicast
(
    collector_id      LowCardinality(String),
    router_ip         IPv6,
    router_sysname    LowCardinality(String),
    peer_ip           IPv6,
    rib               Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn          UInt32,
    peer_bgp_id       IPv4,
    session_id        UInt64,
    seq               UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router         DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags       Array(LowCardinality(String)),
    stream_seq        UInt64 CODEC(DoubleDelta, ZSTD(1)),

    family            LowCardinality(String),
    prefix            String CODEC(ZSTD(3)),
    path_id           UInt32,
    is_withdraw       UInt8,
    origin            UInt8,
    as_path           Array(UInt32),
    next_hop          String,
    med               Nullable(UInt32),
    local_pref        Nullable(UInt32),
    communities       Array(UInt32),
    ext_communities   Array(String),
    route_targets     Array(String),
    large_communities Array(String),

    -- idx_prefix: the sort key above leads with router_ip/peer_ip/rib/ts_router/stream_seq,
    -- so a query/ point lookup for one prefix across routers gets no help
    -- from PRIMARY KEY -- EXPLAIN indexes=1 on such a query reports
    -- `PrimaryKey Condition: true`, every granule a candidate. This bloom
    -- filter lets ClickHouse skip granules whose prefix set provably
    -- excludes the value searched for.
    INDEX idx_prefix prefix TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq, prefix, path_id, is_withdraw)
PRIMARY KEY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS vantage.route_vpn
(
    collector_id      LowCardinality(String),
    router_ip         IPv6,
    router_sysname    LowCardinality(String),
    peer_ip           IPv6,
    rib               Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn          UInt32,
    peer_bgp_id       IPv4,
    session_id        UInt64,
    seq               UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router         DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags       Array(LowCardinality(String)),
    stream_seq        UInt64 CODEC(DoubleDelta, ZSTD(1)),

    family            LowCardinality(String),
    prefix            String CODEC(ZSTD(3)),
    path_id           UInt32,
    is_withdraw       UInt8,
    rd                String,
    labels            Array(UInt32),
    origin            UInt8,
    as_path           Array(UInt32),
    next_hop          String,
    med               Nullable(UInt32),
    local_pref        Nullable(UInt32),
    communities       Array(UInt32),
    ext_communities   Array(String),
    route_targets     Array(String),
    large_communities Array(String),

    -- idx_prefix: same skip index and same reasoning as route_unicast's
    -- above -- a VPN point lookup for one prefix across RDs gets no help
    -- from PRIMARY KEY either.
    INDEX idx_prefix prefix TYPE bloom_filter GRANULARITY 4
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq, prefix, rd, path_id, is_withdraw)
PRIMARY KEY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS vantage.route_evpn
(
    collector_id      LowCardinality(String),
    router_ip         IPv6,
    router_sysname    LowCardinality(String),
    peer_ip           IPv6,
    rib               Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn          UInt32,
    peer_bgp_id       IPv4,
    session_id        UInt64,
    seq               UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router         DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags       Array(LowCardinality(String)),
    stream_seq        UInt64 CODEC(DoubleDelta, ZSTD(1)),

    route_type        UInt8,
    rd                String,
    prefix            String CODEC(ZSTD(3)),
    mac               String,
    ip                String,
    gateway_ip        String,
    ethernet_tag      UInt32,
    esi               String,
    labels            Array(UInt32),
    path_id           UInt32,
    is_withdraw       UInt8,
    origin            UInt8,
    as_path           Array(UInt32),
    next_hop          String,
    med               Nullable(UInt32),
    local_pref        Nullable(UInt32),
    communities       Array(UInt32),
    ext_communities   Array(String),
    route_targets     Array(String),
    large_communities Array(String)
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq, route_type, rd, prefix, mac, ip, ethernet_tag, esi, path_id, is_withdraw)
PRIMARY KEY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS vantage.ls_events
(
    collector_id   LowCardinality(String),
    router_ip      IPv6,
    router_sysname LowCardinality(String),
    peer_ip        IPv6,
    rib            Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn       UInt32,
    peer_bgp_id    IPv4,
    session_id     UInt64,
    seq            UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector   DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags    Array(LowCardinality(String)),
    stream_seq     UInt64 CODEC(DoubleDelta, ZSTD(1)),

    family         LowCardinality(String),
    raw_reach      String,
    raw_unreach    String,
    -- end_of_rib: an LS End-of-RIB (RFC 4724 §2) is an MP_UNREACH with AFI
    -- 16388/SAFI 71 and no NLRI at all -- no raw bytes, no typed node/link
    -- -- so without this column it produced zero rows anywhere, and "the
    -- LS RIB finished converging" was unrepresentable even though a
    -- topology consumer needs it to tell a complete graph from one still
    -- loading.
    --
    -- This stays a column on ls_events rather than moving to eor_events
    -- below with the BGP markers: an LS End-of-RIB is one of the shapes an
    -- ls_events row already takes (the others being raw_reach/raw_unreach
    -- remainders), so the row is not an artifact wedged into a table of
    -- something else the way a marker in route_unicast was.
    --
    -- What that argument does NOT buy: ls_events is still a table where a
    -- marker and an object sit side by side, so any query counting LS
    -- objects here must still exclude end_of_rib = 1 or count 2 of the
    -- archive's 49 ls_events rows as content. The split ended that
    -- discipline for the three BGP route tables and for no one else. Do
    -- not read eor_events' own header below as a claim about this column
    -- -- it is a claim about route_unicast, route_vpn and route_evpn,
    -- which is what it says now.
    end_of_rib     UInt8
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

-- Link-state objects, split by NLRI type because a node, a link and a prefix
-- are different objects with different identities. One table would need a
-- sort key covering three shapes at once, which is the defect that let a
-- merge destroy 16 EVPN routes before the sort keys above were widened.
--
-- node_key/local_node_key/remote_node_key are MATERIALIZED hashes of the full
-- node-descriptor tuple. They are the join key for topology assembly AND the
-- node id Grafana's Node Graph edges reference, so the SAME expression must
-- appear in every table -- a key computed two slightly different ways yields
-- a join that silently matches nothing, i.e. an empty graph rather than an
-- error.
CREATE TABLE IF NOT EXISTS vantage.ls_nodes
(
    collector_id   LowCardinality(String),
    router_ip      IPv6,
    router_sysname LowCardinality(String),
    peer_ip        IPv6,
    rib            Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn       UInt32,
    peer_bgp_id    IPv4,
    session_id     UInt64,
    seq            UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector   DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags    Array(LowCardinality(String)),
    stream_seq     UInt64 CODEC(DoubleDelta, ZSTD(1)),

    protocol       UInt8,
    identifier     UInt64,
    asn            UInt32,
    bgpls_id       UInt32,
    area           UInt32,
    router_id      String,
    -- router_id_v4: the node's IPv4 router-id (BGP-LS Attribute TLV 1028,
    -- RFC 9552 §5.3.1), dotted-quad text, '' when absent. router_id above
    -- stays hex/raw and is NOT replaced by this -- IS-IS system IDs are 6
    -- bytes and not addresses -- but without a rendered address anywhere,
    -- every topology node label was hex, which defeats the goal of a
    -- human-readable topology. Not part of node_key: it is presentation,
    -- not identity (asn/bgpls_id/area/router_id already are).
    router_id_v4   String,
    node_key       UInt64 MATERIALIZED cityHash64(protocol, identifier, asn, bgpls_id, area, router_id),

    is_withdraw    UInt8,
    name           String,
    srgb_base      UInt32,
    srgb_size      UInt32,
    srlb_base      UInt32,
    srlb_size      UInt32,
    sr_algorithms  Array(UInt8),
    unknown_tlvs   Map(UInt16, String)
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq, protocol, identifier, asn, bgpls_id, area, router_id, is_withdraw)
PRIMARY KEY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS vantage.ls_links
(
    collector_id   LowCardinality(String),
    router_ip      IPv6,
    router_sysname LowCardinality(String),
    peer_ip        IPv6,
    rib            Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn       UInt32,
    peer_bgp_id    IPv4,
    session_id     UInt64,
    seq            UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector   DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags    Array(LowCardinality(String)),
    stream_seq     UInt64 CODEC(DoubleDelta, ZSTD(1)),

    protocol          UInt8,
    identifier        UInt64,
    local_asn         UInt32,
    local_bgpls_id    UInt32,
    local_area        UInt32,
    local_router_id   String,
    remote_asn        UInt32,
    remote_bgpls_id   UInt32,
    remote_area       UInt32,
    remote_router_id  String,
    local_ifaddr      String,
    remote_ifaddr     String,
    link_local_id     UInt32,
    link_remote_id    UInt32,
    local_node_key    UInt64 MATERIALIZED cityHash64(protocol, identifier, local_asn, local_bgpls_id, local_area, local_router_id),
    remote_node_key   UInt64 MATERIALIZED cityHash64(protocol, identifier, remote_asn, remote_bgpls_id, remote_area, remote_router_id),

    is_withdraw       UInt8,
    -- Parallel arrays, index-aligned: one link may carry several adjacency
    -- SIDs (RFC 9085 2.2.1), typically a protected/unprotected pair.
    adj_sids          Array(UInt32),
    adj_sid_flags     Array(UInt8),
    adj_sid_weights   Array(UInt8),
    te_metric         UInt32,
    igp_metric        UInt32,
    admin_group       UInt32,
    max_bandwidth     Float32,
    unknown_tlvs      Map(UInt16, String)
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq, protocol, identifier, local_asn, local_bgpls_id, local_area, local_router_id, remote_asn, remote_bgpls_id, remote_area, remote_router_id, local_ifaddr, remote_ifaddr, link_local_id, link_remote_id, is_withdraw)
PRIMARY KEY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

-- IPv4 and IPv6 prefix NLRI (types 3 and 4) are both decoded into this
-- table, including the Prefix-SID that carries node SIDs. See
-- bgp/linkstate.go's lsNLRIPrefix.
CREATE TABLE IF NOT EXISTS vantage.ls_prefixes
(
    collector_id   LowCardinality(String),
    router_ip      IPv6,
    router_sysname LowCardinality(String),
    peer_ip        IPv6,
    rib            Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn       UInt32,
    peer_bgp_id    IPv4,
    session_id     UInt64,
    seq            UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector   DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags    Array(LowCardinality(String)),
    stream_seq     UInt64 CODEC(DoubleDelta, ZSTD(1)),

    protocol       UInt8,
    identifier     UInt64,
    asn            UInt32,
    bgpls_id       UInt32,
    area           UInt32,
    router_id      String,
    prefix         String CODEC(ZSTD(3)),
    prefix_len     UInt8,
    node_key       UInt64 MATERIALIZED cityHash64(protocol, identifier, asn, bgpls_id, area, router_id),

    is_withdraw    UInt8,
    prefix_sid     UInt32,
    -- prefix_sid_flags changes what prefix_sid MEANS: with RFC 9085's V and L
    -- flags set the value is an absolute label rather than an index into the
    -- node's SRGB, so a reader that ignores the flags can be wrong by the
    -- SRGB base. has_prefix_sid separates an absent TLV from index 0, which
    -- is a legal SID.
    prefix_sid_flags UInt8,
    has_prefix_sid   UInt8,
    prefix_metric  UInt32,
    prefix_attr_flags UInt8,
    ospf_route_type   UInt8,
    unknown_tlvs   Map(UInt16, String)
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq, protocol, identifier, asn, bgpls_id, area, router_id, prefix, prefix_len, is_withdraw)
PRIMARY KEY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS vantage.peer_events
(
    collector_id   LowCardinality(String),
    router_ip      IPv6,
    router_sysname LowCardinality(String),
    peer_ip        IPv6,
    rib            Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn       UInt32,
    peer_bgp_id    IPv4,
    session_id     UInt64,
    seq            UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector   DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags    Array(LowCardinality(String)),
    stream_seq     UInt64 CODEC(DoubleDelta, ZSTD(1)),

    -- 'view_lost' is the collector saying it stopped being able to see this
    -- peer, and it is NOT 'down'. 'down' is the router stating a BGP session
    -- dropped, carried on the wire with a reason code (RFC 7854 sec 4.9);
    -- 'view_lost' is emitted by the collector when the BMP transport itself
    -- ends, where the router said nothing and there is no reason to record
    -- (down_reason is 0 on every such row). Keeping them apart is what lets
    -- "one fleet's routers turned over 354 BMP sessions and sent 0 Peer
    -- Downs" stay a readable fact rather than 354 fabricated router
    -- statements. See collector's Session.Close.
    kind           Enum8('unspecified' = 0, 'up' = 1, 'down' = 2, 'view_lost' = 3),
    local_ip       IPv6,
    local_port     UInt16,
    remote_port    UInt16,
    down_reason    UInt32,
    cap_four_byte_as UInt8,

    -- The session facts the wire was already carrying, and had been
    -- decoding and discarding: collector's capsProto always built
    -- mp_families and addpath_families from both of a session's OPEN
    -- messages, and bmp/tlv.go always decoded the Initiation sysDescr TLV,
    -- but sink persisted neither. There is no keepalive column: RFC 4271's
    -- OPEN carries no such value, and hold_time/3 is a convention rather
    -- than an observation.
    --
    -- hold_time_seen exists because 0 IS A HOLD TIME -- RFC 4271 sec 4.2,
    -- the timer never expires, keepalives off -- and a UInt16 alone cannot
    -- tell that from a row where no OPEN was observed.
    hold_time        UInt16 DEFAULT 0,
    hold_time_seen   UInt8  DEFAULT 0,
    mp_families      Array(LowCardinality(String)) DEFAULT [],
    addpath_families Array(LowCardinality(String)) DEFAULT [],

    -- The Initiation message's sysDescr TLV (type 1), which is the vendor
    -- and version string a router states about itself. router_sysname above
    -- is the type-2 sysName; the two are different TLVs and this archive
    -- stored only the second.
    sys_descr        String DEFAULT ''
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

-- eor_events: BGP End-of-RIB markers, one row per (router, peer, rib,
-- session, family).
--
-- These were previously filed as rows in route_unicast carrying
-- end_of_rib = 1, which had two consequences worth stating plainly, since
-- the shape looked reasonable for a year:
--
--   1. Every query over a route table had to remember `end_of_rib = 0`
--      forever, and forgetting it is invisible -- the marker is a real row
--      with a real prefix-shaped key. Counting collection artifacts as
--      routes is this project's single most recurring defect class: five
--      separate dashboard panels shipped it, and the query layer needs the
--      predicate in every statement.
--   2. A marker for a family that has no unicast table -- EVPN, VPN --
--      still had to land in route_unicast, so route_unicast contained rows
--      whose family column read 'evpn'. The dump-progress question for
--      those families was answerable only by querying the unicast table for
--      non-unicast rows, which nothing did, so dump_state read 'unknown'
--      forever for a VPN- or EVPN-only peer.
--
-- Splitting the artifact out of the data makes both problems structural
-- rather than remembered for the three BGP route tables: route_unicast,
-- route_vpn and route_evpn now contain only routes, and dump-progress is a
-- question with its own table and its own family column.
--
-- The scope of that claim is worth stating exactly, because it is smaller
-- than "the defect class is dead". ls_events still carries its own
-- end_of_rib column (see it, ~200 lines above), so BGP-LS markers are still
-- filed beside BGP-LS objects -- 2 such rows in the archive -- and a query
-- counting LS objects still has to exclude them by hand. What ended is the
-- discipline the ROUTE tables demanded, not the shape everywhere it occurs.
--
-- Plain MergeTree, not ReplacingMergeTree, and the reason is the opposite
-- of the one first written here.
--
-- Under ReplacingMergeTree a redelivered marker leaves count() wrong until
-- the next merge and right afterward -- intermittently correct, which is
-- the worst of the three states, because an operator who checks twice gets
-- two answers and neither is labeled. Under plain MergeTree count() is
-- wrong permanently and identically on every run. That is the choice being
-- made: counting rows in this table is never safe, so nothing can come to
-- depend on it being safe. Every consumer asks the marker's EXISTENCE
-- instead -- `max(is_marker)`, a GROUP BY, an EXISTS -- and gets the same
-- answer whether the table holds one copy or five. See query/query.go's
-- eorCTE and query/peers.go's dump_families, both of which say so at
-- length, and the fixture that writes a byte-identical duplicate marker on
-- purpose.
--
-- A redelivered marker is therefore harmless HERE while being permanently
-- visible in a row count. Both halves of that are intended.
CREATE TABLE IF NOT EXISTS vantage.eor_events
(
    collector_id   LowCardinality(String),
    router_ip      IPv6,
    router_sysname LowCardinality(String),
    peer_ip        IPv6,
    rib            Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn       UInt32,
    peer_bgp_id    IPv4,
    session_id     UInt64,
    seq            UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector   DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags    Array(LowCardinality(String)),
    stream_seq     UInt64 CODEC(DoubleDelta, ZSTD(1)),
    family         LowCardinality(String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, session_id, family, seq, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS vantage.stats_events
(
    collector_id   LowCardinality(String),
    router_ip      IPv6,
    router_sysname LowCardinality(String),
    peer_ip        IPv6,
    rib            Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    peer_asn       UInt32,
    peer_bgp_id    IPv4,
    session_id     UInt64,
    seq            UInt64 CODEC(DoubleDelta, ZSTD(1)),
    ts_router      DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    ts_collector   DateTime64(6) CODEC(DoubleDelta, ZSTD(1)),
    parse_flags    Array(LowCardinality(String)),
    stream_seq     UInt64 CODEC(DoubleDelta, ZSTD(1)),

    counters       Map(UInt32, UInt64)
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(ts_collector)
ORDER BY (router_ip, peer_ip, rib, ts_router, stream_seq)
TTL toDateTime(ts_collector) + INTERVAL 90 DAY;

-- Current state. History tables expire after retention; these do not,
-- because BMP only sends changes and a route, session or dump marker that
-- stays stable for longer than retention is still live. Each is fed by a
-- materialized view on its history table, keyed on collector_id and
-- session_id: session ids are minted per collector, and a key without the
-- session lets a superseded session that keeps publishing overwrite live
-- state (measured 2026-09-05: 200,460 of 1,000,000 routes lost). One
-- partition (PARTITION BY tuple()): ClickHouse deduplicates only within a
-- partition, and one object's rows can span months. Superseded sessions
-- are pruned by the writer's cleanup, not by TTL, since a TTL cannot see
-- another table.
--
-- `CREATE TABLE ... AS vantage.<history>` copies the history table's
-- columns, codecs, MATERIALIZED columns and skip indexes. With an explicit
-- ENGINE it does not copy the TTL, but since ClickHouse 24.9 it does copy
-- any PARTITION BY, PRIMARY KEY, ORDER BY or SAMPLE BY the statement leaves
-- out. So each current table states all three keys itself: `PARTITION BY
-- tuple()` is one partition, the same as none, and PRIMARY KEY repeats the
-- ORDER BY (the history table's shorter PRIMARY KEY is not a prefix of it,
-- which the server rejects). sink's
-- TestCurrentTablesAsCreatedHaveNoPartitionOrTTL checks the tables the
-- server actually creates. A `SELECT *` view does not carry MATERIALIZED
-- columns (node_key and the two link endpoint keys); the current table
-- recomputes them from the same expression. A `SELECT *` view reads its
-- history table's columns at each insert and writes them by name, so a
-- column added to a history table later reaches its current table once the
-- current table has the column too, and is dropped until then (measured on
-- 24.8 and 26.8; see deploy/clickhouse/migrations/README.md).

-- Every peer event of the retained sessions, so the peer state queries run
-- unchanged. A session's peer events are few (ups, downs, flaps).
CREATE TABLE IF NOT EXISTS vantage.peer_current AS vantage.peer_events
ENGINE = ReplacingMergeTree
PARTITION BY tuple()
ORDER BY (collector_id, router_ip, session_id, peer_ip, rib, ts_router, stream_seq)
PRIMARY KEY (collector_id, router_ip, session_id, peer_ip, rib, ts_router, stream_seq);

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.peer_current_mv TO vantage.peer_current
AS SELECT * FROM vantage.peer_events;

-- Did an End-of-RIB arrive, per (peer view, session, family). Existence is
-- the whole fact. Link state's marker rides ls_events.end_of_rib and is
-- filed here as family 'ls'.
CREATE TABLE IF NOT EXISTS vantage.eor_current
(
    collector_id   LowCardinality(String),
    router_ip      IPv6,
    peer_ip        IPv6,
    rib            Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3, 'loc_rib' = 4),
    session_id     UInt64,
    family         LowCardinality(String)
)
ENGINE = ReplacingMergeTree
ORDER BY (collector_id, router_ip, session_id, peer_ip, rib, family);

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.eor_current_mv TO vantage.eor_current
AS SELECT collector_id, router_ip, peer_ip, rib, session_id, family FROM vantage.eor_events;

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.eor_current_ls_mv TO vantage.eor_current
AS SELECT collector_id, router_ip, peer_ip, rib, session_id, 'ls' AS family
FROM vantage.ls_events WHERE end_of_rib = 1;

-- One row per route per session, the newest by seq (monotonic within a
-- session and peer view, both of which are in the key). Withdrawals are
-- kept as tombstones so a later update replaces the right row. No TTL: a
-- TTL can delete a withdrawal before it merges over the announcement it
-- replaced, which reads the withdrawn route as live again. Rows leave when
-- their session is superseded and cleaned up.
CREATE TABLE IF NOT EXISTS vantage.route_unicast_current AS vantage.route_unicast
ENGINE = ReplacingMergeTree(seq)
PARTITION BY tuple()
ORDER BY (collector_id, router_ip, session_id, peer_ip, rib, family, prefix, path_id)
PRIMARY KEY (collector_id, router_ip, session_id, peer_ip, rib, family, prefix, path_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.route_unicast_current_mv TO vantage.route_unicast_current
AS SELECT * FROM vantage.route_unicast;

CREATE TABLE IF NOT EXISTS vantage.route_vpn_current AS vantage.route_vpn
ENGINE = ReplacingMergeTree(seq)
PARTITION BY tuple()
ORDER BY (collector_id, router_ip, session_id, peer_ip, rib, family, rd, prefix, path_id)
PRIMARY KEY (collector_id, router_ip, session_id, peer_ip, rib, family, rd, prefix, path_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.route_vpn_current_mv TO vantage.route_vpn_current
AS SELECT * FROM vantage.route_vpn;

CREATE TABLE IF NOT EXISTS vantage.route_evpn_current AS vantage.route_evpn
ENGINE = ReplacingMergeTree(seq)
PARTITION BY tuple()
ORDER BY (collector_id, router_ip, session_id, peer_ip, rib, route_type, rd, prefix, mac, ip, ethernet_tag, esi, path_id)
PRIMARY KEY (collector_id, router_ip, session_id, peer_ip, rib, route_type, rd, prefix, mac, ip, ethernet_tag, esi, path_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.route_evpn_current_mv TO vantage.route_evpn_current
AS SELECT * FROM vantage.route_evpn;

-- The link-state objects, keyed the same way on each table's own object
-- identity: the node descriptor, the full link descriptor, and the node
-- descriptor plus prefix.
CREATE TABLE IF NOT EXISTS vantage.ls_nodes_current AS vantage.ls_nodes
ENGINE = ReplacingMergeTree(seq)
PARTITION BY tuple()
ORDER BY (collector_id, router_ip, session_id, peer_ip, rib, protocol, identifier, asn, bgpls_id, area, router_id)
PRIMARY KEY (collector_id, router_ip, session_id, peer_ip, rib, protocol, identifier, asn, bgpls_id, area, router_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.ls_nodes_current_mv TO vantage.ls_nodes_current
AS SELECT * FROM vantage.ls_nodes;

CREATE TABLE IF NOT EXISTS vantage.ls_links_current AS vantage.ls_links
ENGINE = ReplacingMergeTree(seq)
PARTITION BY tuple()
ORDER BY (collector_id, router_ip, session_id, peer_ip, rib, protocol, identifier, local_asn, local_bgpls_id, local_area, local_router_id, remote_asn, remote_bgpls_id, remote_area, remote_router_id, local_ifaddr, remote_ifaddr, link_local_id, link_remote_id)
PRIMARY KEY (collector_id, router_ip, session_id, peer_ip, rib, protocol, identifier, local_asn, local_bgpls_id, local_area, local_router_id, remote_asn, remote_bgpls_id, remote_area, remote_router_id, local_ifaddr, remote_ifaddr, link_local_id, link_remote_id);

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.ls_links_current_mv TO vantage.ls_links_current
AS SELECT * FROM vantage.ls_links;

CREATE TABLE IF NOT EXISTS vantage.ls_prefixes_current AS vantage.ls_prefixes
ENGINE = ReplacingMergeTree(seq)
PARTITION BY tuple()
ORDER BY (collector_id, router_ip, session_id, peer_ip, rib, protocol, identifier, asn, bgpls_id, area, router_id, prefix, prefix_len)
PRIMARY KEY (collector_id, router_ip, session_id, peer_ip, rib, protocol, identifier, asn, bgpls_id, area, router_id, prefix, prefix_len);

CREATE MATERIALIZED VIEW IF NOT EXISTS vantage.ls_prefixes_current_mv TO vantage.ls_prefixes_current
AS SELECT * FROM vantage.ls_prefixes;

-- Which writer may run superseded-session cleanup until when. Each writer
-- writes a claim and reads the table back, and wins only if no other writer
-- has a live claim since the newest release (holder ''); see
-- sink/cleanup.go. That relies on a single ClickHouse server: on replicated
-- or cloud ClickHouse two writers can both run a cycle, which only repeats
-- idempotent deletes. A missed lease only delays reclaiming disk: cleanup
-- never changes an answer.
CREATE TABLE IF NOT EXISTS vantage.cleanup_lease
(
    holder     String,
    expires_at DateTime64(3)
)
ENGINE = MergeTree ORDER BY expires_at
TTL toDateTime(expires_at) + INTERVAL 1 DAY;

-- Collector liveness. Every collector publishes a CollectorBeat at startup and
-- every 30 s after; the writer stores one row per beat, and ReplacingMergeTree
-- collapses a (collector, process start) to its newest beat on merge. Readers
-- take max() per collector, so an unmerged pile of beats reads the same as a
-- merged one and nothing here needs FINAL.
--
-- inserted_at is the ClickHouse clock at insert, and it is the only column
-- liveness is decided on: a collector is stale when its newest inserted_at is
-- older than the threshold, compared with now64() in the reading query -- one
-- clock on both sides. beat_at is the collector's own clock, kept for display
-- and never compared, so a collector with a wrong clock cannot make itself
-- stale or fresh. started_at is the collector process's start on the
-- collector's clock, the clock its session_ids come from; the newest one per
-- collector is its epoch, and a current session with a lower session_id
-- belongs to a process that has since been replaced.
--
-- No TTL: a handful of rows per collector, and they keep the last-heard and
-- started-at of a collector that is gone. Not in the writer's cleanup, which
-- is keyed by session; `vantage purge` removes a retired collector's rows.
CREATE TABLE IF NOT EXISTS vantage.collector_beats
(
    collector_id LowCardinality(String),
    started_at   DateTime64(9, 'UTC'),
    beat_at      DateTime64(3, 'UTC'),
    inserted_at  DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(inserted_at)
ORDER BY (collector_id, started_at);
