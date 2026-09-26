// Package sink's clickhouse.go is the only piece of this package that talks
// to a database. rows.go stays pure so schema fidelity can be tested without
// infrastructure; this file is where that fidelity is enforced against a
// live connection.
package sink

import (
	"context"
	"fmt"
	"regexp"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/jp2195/vantage/redact"
	"github.com/jp2195/vantage/secret"
)

// ExpectedSchemaVersion is the version of deploy/clickhouse/schema.sql this
// binary was built against. NewClickHouse refuses to connect to anything else:
// a silent column mismatch is the failure most likely to corrupt data without
// anyone noticing, so it is made loud and fatal at startup instead.
//
// Version 1 is the public baseline: the schema was squashed before the first
// release, folding every earlier step into schema.sql's CREATE statements.
// Version 2 adds collector_beats, the collector heartbeat table, moved to by
// deploy/clickhouse/migrations/001-collector-beats.sql.
//
// Bumping this constant and schema.sql's own version INSERT together, with
// a migration that moves an existing database the same step, is what makes
// a shape change safe: a writer built for 2 refuses to start against a
// database still carrying version 1's tables rather than emitting inserts
// ClickHouse will reject for the life of the consumer.
// TestExpectedSchemaVersion and
// TestSchemaAndItsHighestMigrationDeclareTheSameVersion fail if this
// constant moves without the migration beside it, so neither half can
// happen quietly.
const ExpectedSchemaVersion = 2

type ClickHouse struct {
	conn driver.Conn
	// db is the ClickHouse database every statement below is qualified
	// with, taken from the DSN rather than hardcoded to "vantage". That is
	// what lets this package's live tests run against a database of their
	// own (see clickhouse_test.go's testDB and chtest.DSN) instead of
	// writing fixture rows into the archive the dashboards read.
	db string
}

// dbNameRe is the shape a database name has to have to be interpolated into
// the statements below. The name arrives from an operator-supplied DSN and
// cannot be bound as a parameter -- ClickHouse has no placeholder for an
// identifier -- so it is validated to a plain unquoted identifier once, at
// construction, rather than trusted at each of the seven call sites.
var dbNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NewClickHouse dials a ClickHouse from an operator-supplied DSN.
//
// It takes a secret.ClickHouseDSN rather than a string, and this is the one
// place in the tree that calls RevealSecret on one (the other is
// natsutil.Connect). Two things follow from that, both deliberate:
//
//   - The caller cannot accidentally print the DSN it passed here, because
//     it does not have a printable one to pass.
//
//   - Every error this function returns is safe to print. All but one have
//     been through redact.Err; the exception is the http_proxy rejection
//     below, which redact.Err is not what makes safe -- it is built from
//     an already-redacted value and never sees the raw one. So a caller
//     writing the obvious
//
//     fmt.Errorf("connect clickhouse %s: %w", cfg.ClickHouseDSN, err)
//
//     is safe in both of its operands without having to know that. A DSN
//     clickhouse-go rejects surfaces as a *url.Error carrying the raw
//     string (lib/churl builds a literal net/url.Error), which is exactly
//     what redact.Err recognizes structurally.
func NewClickHouse(ctx context.Context, dsn secret.ClickHouseDSN) (*ClickHouse, error) {
	// The one place redact.Err cannot reach, so the driver is never given
	// the value at all. clickhouse-go parses the "http_proxy" parameter as
	// a URL of its own and formats the resulting *url.Error with "%s", not
	// "%w" -- flattening it to text that wraps nothing, so errors.As finds
	// no *url.Error and the proxy's password survives into the startup
	// error verbatim. Pre-validating here, rather than scrubbing that text
	// afterwards, is the approach that has actually held in this tree; see
	// redact.CheckClickHouseProxy, and redact's package comment
	// for why scrubbing was abandoned.
	//
	// Deliberately this narrow check and not dsn.Validate(): full DSN
	// validation is net/url's grammar, and clickhouse-go parses with
	// lib/churl, a fork of it. Rejecting here on a grammar divergence
	// would turn a hypothetical availability difference into a startup
	// failure for no safety gain, since churl's own parse failures are
	// *url.Error and redact.Err handles those. vantage-writer still calls
	// Validate at startup for the fast, clear failure.
	if err := redact.CheckClickHouseProxy(dsn.RevealSecret()); err != nil {
		return nil, fmt.Errorf("clickhouse: parse dsn: %w", err)
	}
	opts, err := clickhouse.ParseDSN(dsn.RevealSecret())
	if err != nil {
		return nil, fmt.Errorf("clickhouse: parse dsn: %w", redact.Err(err))
	}
	// The DSN names the database this writer inserts into. An absent one is
	// rejected rather than defaulted: clickhouse-go leaves Auth.Database
	// empty for a path-less DSN and lets the server fall back to "default",
	// which is never where deploy/clickhouse/schema.sql has been applied,
	// so the fallback could only ever produce a confusing "no such table"
	// at insert time -- long after startup, and only for whichever table
	// happened to receive the first row.
	db := opts.Auth.Database
	if !dbNameRe.MatchString(db) {
		return nil, fmt.Errorf("clickhouse: dsn database %q is not a plain "+
			"identifier -- name the database in the DSN path (e.g. "+
			"clickhouse://host:9000/vantage)", db)
	}
	// Inserts are synchronous unless the DSN asks otherwise. ClickHouse
	// turned async_insert on by default in 26.2, which buffers every INSERT
	// on the server and, with the default wait_for_async_insert = 1, holds
	// it until the buffer flushes, after its 50 to 200 ms busy timeout.
	// Measured on 26.8.7 over twenty one-row inserts: 55 ms on average
	// asynchronous, 1 ms synchronous. Insert issues one statement per table
	// in turn, and the writer already batches, so the server's buffer adds
	// only that wait. It would also make the ack depend on
	// wait_for_async_insert, which a user profile can turn off: an INSERT
	// that returns before its rows are written breaks Insert's contract that
	// nil means stored. A DSN that sets async_insert itself
	// (?async_insert=1) keeps its own choice.
	if _, set := opts.Settings["async_insert"]; !set {
		if opts.Settings == nil {
			opts.Settings = clickhouse.Settings{}
		}
		opts.Settings["async_insert"] = 0
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open: %w", redact.Err(err))
	}
	if err := conn.Ping(ctx); err != nil {
		// clickhouse.Open already started a background pool-drain goroutine
		// (holding a time.Ticker) even though the connection never proved
		// live: only Close stops it. Every transient Ping failure without
		// this leaks one goroutine and one ticker permanently.
		conn.Close()
		return nil, fmt.Errorf("clickhouse: ping: %w", redact.Err(err))
	}
	c := &ClickHouse{conn: conn, db: db}
	if err := c.checkSchema(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *ClickHouse) checkSchema(ctx context.Context) error {
	var got uint32
	row := c.conn.QueryRow(ctx,
		"SELECT max(version) FROM "+c.db+".schema_version")
	if err := row.Scan(&got); err != nil {
		return fmt.Errorf("clickhouse: reading %s.schema_version (has "+
			"deploy/clickhouse/schema.sql been applied to it?): %w", c.db, err)
	}
	if got != ExpectedSchemaVersion {
		return fmt.Errorf("clickhouse: schema version %d, this binary expects "+
			"%d -- refusing to insert into an unexpected shape", got,
			ExpectedSchemaVersion)
	}
	return nil
}

func (c *ClickHouse) Close() error { return c.conn.Close() }

// Conn and DB expose what this type dialed, for a reader that needs to run
// its own SELECTs against the same database.
//
// They exist for cmd/vantage-api, which builds a query.Q over them. That
// daemon could have dialed ClickHouse itself -- it writes nothing, so none
// of the insert machinery above is any use to it -- and reusing this
// constructor instead is deliberate: NewClickHouse owns the DSN redaction
// (see its doc comment, and the history of leak fixes behind it) and
// the ExpectedSchemaVersion check. A second dial path would re-derive both,
// and a read API that skipped the version check would serve confident
// answers out of a database whose columns had moved.
//
// Conn hands out the pool rather than a copy, so a caller can exhaust it;
// that is the same bargain every user of a *sql.DB-shaped handle makes, and
// the alternative -- a second pool for the same process -- is worse. DB is
// returned rather than exported as a field because it is read from the DSN
// at construction and must not be assignable afterward: every statement in
// this file is qualified with it, and a caller that could change it would
// be redirecting inserts, not just reads.
func (c *ClickHouse) Conn() driver.Conn { return c.conn }

// DB is the database name taken from the DSN.
func (c *ClickHouse) DB() string { return c.db }

// Insert writes every row in one batch per table. It returns nil only when
// ClickHouse has accepted all of them: the caller acks on a nil return and on
// nothing else, which is what makes the pipeline lossless.
func (c *ClickHouse) Insert(ctx context.Context, rows Rows) error {
	if err := c.insertUnicast(ctx, rows.Unicast); err != nil {
		return err
	}
	if err := c.insertVpn(ctx, rows.Vpn); err != nil {
		return err
	}
	if err := c.insertEvpn(ctx, rows.Evpn); err != nil {
		return err
	}
	if err := c.insertEor(ctx, rows.Eor); err != nil {
		return err
	}
	if err := c.insertLs(ctx, rows.Ls); err != nil {
		return err
	}
	if err := c.insertLsNodes(ctx, rows.LsNodes); err != nil {
		return err
	}
	if err := c.insertLsLinks(ctx, rows.LsLinks); err != nil {
		return err
	}
	if err := c.insertLsPrefixes(ctx, rows.LsPrefixes); err != nil {
		return err
	}
	if err := c.insertPeer(ctx, rows.Peer); err != nil {
		return err
	}
	if err := c.insertStats(ctx, rows.Stats); err != nil {
		return err
	}
	return c.insertBeats(ctx, rows.Beats)
}

func (c *ClickHouse) insertUnicast(ctx context.Context, rows []UnicastRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".route_unicast")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare route_unicast: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Family, r.Prefix, r.PathID, r.IsWithdraw,
			r.Origin, r.ASPath, r.NextHop, r.MED, r.LocalPref,
			r.Communities, r.ExtCommunities, r.RouteTargets, r.LargeCommunities,
		); err != nil {
			return fmt.Errorf("clickhouse: append route_unicast: %w", err)
		}
	}
	return b.Send()
}

// The argument order above is positional and must match
// deploy/clickhouse/schema.sql column-for-column. It has already drifted once
// per column added (rib, then route_targets), each time silently, because
// nothing in Go's type system connects the two. When adding a column, change
// the DDL, the row struct, the translation and this call in one commit, and
// check the count: route_unicast has 26 columns (it had 27 until end_of_rib
// moved to eor_events).

func (c *ClickHouse) insertVpn(ctx context.Context, rows []VpnRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".route_vpn")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare route_vpn: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Family, r.Prefix, r.PathID, r.IsWithdraw, r.RD, r.Labels,
			r.Origin, r.ASPath, r.NextHop, r.MED, r.LocalPref,
			r.Communities, r.ExtCommunities, r.RouteTargets, r.LargeCommunities,
		); err != nil {
			return fmt.Errorf("clickhouse: append route_vpn: %w", err)
		}
	}
	return b.Send()
}

// route_vpn has 28 columns; see the note above insertUnicast.

func (c *ClickHouse) insertEvpn(ctx context.Context, rows []EvpnRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".route_evpn")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare route_evpn: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.RouteType, r.RD, r.Prefix, r.MAC, r.IP, r.GatewayIP, r.EthernetTag,
			r.ESI, r.Labels, r.PathID, r.IsWithdraw,
			r.Origin, r.ASPath, r.NextHop, r.MED, r.LocalPref,
			r.Communities, r.ExtCommunities, r.RouteTargets, r.LargeCommunities,
		); err != nil {
			return fmt.Errorf("clickhouse: append route_evpn: %w", err)
		}
	}
	return b.Send()
}

// route_evpn has 33 columns; see the note above insertUnicast.

func (c *ClickHouse) insertEor(ctx context.Context, rows []EorRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".eor_events")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare eor_events: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Family,
		); err != nil {
			return fmt.Errorf("clickhouse: append eor_events: %w", err)
		}
	}
	return b.Send()
}

// eor_events has 14 columns: the 13 envelope columns plus family. See the
// note above insertUnicast.

func (c *ClickHouse) insertLs(ctx context.Context, rows []LsRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".ls_events")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare ls_events: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Family, r.RawReach, r.RawUnreach, r.EndOfRIB,
		); err != nil {
			return fmt.Errorf("clickhouse: append ls_events: %w", err)
		}
	}
	return b.Send()
}

// ls_events has 17 columns (end_of_rib is one of them); see the note above
// insertUnicast.

func (c *ClickHouse) insertLsNodes(ctx context.Context, rows []LsNodeRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".ls_nodes")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare ls_nodes: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Protocol, r.Identifier, r.ASN, r.BgplsID, r.Area, r.RouterID, r.RouterIDv4,
			r.IsWithdraw, r.Name, r.SrgbBase, r.SrgbSize, r.SrlbBase, r.SrlbSize,
			r.SrAlgorithms, r.UnknownTLVs,
		); err != nil {
			return fmt.Errorf("clickhouse: append ls_nodes: %w", err)
		}
	}
	return b.Send()
}

// ls_nodes has 28 non-MATERIALIZED columns (router_id_v4 among them):
// 13 envelope + 15 own (protocol, identifier, asn, bgpls_id, area, router_id,
// router_id_v4, is_withdraw, name, srgb_base, srgb_size, srlb_base,
// srlb_size, sr_algorithms, unknown_tlvs). node_key is MATERIALIZED by
// ClickHouse from (protocol, identifier, asn, bgpls_id, area, router_id) and
// must NOT appear in this Append list -- see the note above insertUnicast
// for what an argument-count mismatch does here.

func (c *ClickHouse) insertLsLinks(ctx context.Context, rows []LsLinkRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".ls_links")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare ls_links: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Protocol, r.Identifier,
			r.LocalASN, r.LocalBgplsID, r.LocalArea, r.LocalRouterID,
			r.RemoteASN, r.RemoteBgplsID, r.RemoteArea, r.RemoteRouterID,
			r.LocalIfAddr, r.RemoteIfAddr, r.LinkLocalID, r.LinkRemoteID,
			r.IsWithdraw, r.AdjSIDs, r.AdjSIDFlags, r.AdjSIDWeights,
			r.TEMetric, r.IGPMetric, r.AdminGroup, r.MaxBandwidth, r.UnknownTLVs,
		); err != nil {
			return fmt.Errorf("clickhouse: append ls_links: %w", err)
		}
	}
	return b.Send()
}

// ls_links has 36 non-MATERIALIZED columns: 13 envelope + 23 own (protocol,
// identifier, local_asn, local_bgpls_id, local_area, local_router_id,
// remote_asn, remote_bgpls_id, remote_area, remote_router_id, local_ifaddr,
// remote_ifaddr, link_local_id, link_remote_id, is_withdraw, adj_sids,
// adj_sid_flags, adj_sid_weights, te_metric, igp_metric, admin_group,
// max_bandwidth, unknown_tlvs). local_node_key and remote_node_key are
// MATERIALIZED and must NOT appear in this Append list -- see the note above
// insertUnicast.

func (c *ClickHouse) insertLsPrefixes(ctx context.Context, rows []LsPrefixRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".ls_prefixes")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare ls_prefixes: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Protocol, r.Identifier, r.ASN, r.BgplsID, r.Area, r.RouterID,
			r.Prefix, r.PrefixLen,
			r.IsWithdraw, r.PrefixSID, r.PrefixSIDFlags, r.HasPrefixSID,
			r.PrefixMetric, r.PrefixAttrFlags, r.OSPFRouteType, r.UnknownTLVs,
		); err != nil {
			return fmt.Errorf("clickhouse: append ls_prefixes: %w", err)
		}
	}
	return b.Send()
}

// ls_prefixes has 29 non-MATERIALIZED columns: 13 envelope + 16 own
// (protocol, identifier, asn, bgpls_id, area, router_id, prefix, prefix_len,
// is_withdraw, prefix_sid, prefix_sid_flags, has_prefix_sid, prefix_metric,
// prefix_attr_flags, ospf_route_type, unknown_tlvs). node_key is MATERIALIZED
// from (protocol, identifier, asn, bgpls_id, area, router_id) -- the same
// expression ls_nodes uses, which is what lets a prefix join to its node --
// and must NOT appear in this Append list; see the note above insertUnicast
// for what an argument-count mismatch does here.

func (c *ClickHouse) insertPeer(ctx context.Context, rows []PeerRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".peer_events")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare peer_events: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Kind, r.LocalIP, r.LocalPort, r.RemotePort, r.DownReason,
			r.CapFourByteAS,
			r.HoldTime, r.HoldTimeSeen, r.MPFamilies, r.AddPathFamilies,
			r.SysDescr,
		); err != nil {
			return fmt.Errorf("clickhouse: append peer_events: %w", err)
		}
	}
	return b.Send()
}

// peer_events has 24 columns: 13 envelope + 11 own (kind, local_ip,
// local_port, remote_port, down_reason, cap_four_byte_as, hold_time,
// hold_time_seen, mp_families, addpath_families, sys_descr). The last five
// were the most recent additions; see the note above insertUnicast for what an
// argument-count mismatch does here, which is worth re-reading before adding
// a twelfth -- Append is positional, so a column added to schema.sql and
// forgotten here silently shifts every value after it.

func (c *ClickHouse) insertStats(ctx context.Context, rows []StatsRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, "INSERT INTO "+c.db+".stats_events")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare stats_events: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(
			r.CollectorID, r.RouterIP, r.RouterSysname, r.PeerIP, r.RIB, r.PeerASN,
			r.PeerBGPID, r.SessionID, r.Seq, r.TsRouter, r.TsCollector,
			r.ParseFlags, r.StreamSeq,
			r.Counters,
		); err != nil {
			return fmt.Errorf("clickhouse: append stats_events: %w", err)
		}
	}
	return b.Send()
}

// stats_events has 14 columns; see the note above insertUnicast.

// insertBeats names its columns, unlike every inserter above. collector_beats'
// fourth column, inserted_at, must take its DEFAULT -- the ClickHouse clock at
// insert, which is what liveness is measured on -- and a positional insert
// would have to supply a value for it. The argument count still has to match
// the three named columns, so a column added here without the other is still
// a loud error.
func (c *ClickHouse) insertBeats(ctx context.Context, rows []BeatRow) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx,
		"INSERT INTO "+c.db+".collector_beats (collector_id, started_at, beat_at)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare collector_beats: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(r.CollectorID, r.StartedAt, r.BeatAt); err != nil {
			return fmt.Errorf("clickhouse: append collector_beats: %w", err)
		}
	}
	return b.Send()
}
