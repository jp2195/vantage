-- 001: schema version 1 to 2. Adds collector_beats, the collector heartbeat
-- table (see its own comment at the end of schema.sql for what each column
-- is and why liveness is read from inserted_at).
--
-- Metadata only: it creates one empty table and touches no existing row, so
-- it is safe against a live archive. The CREATE is schema.sql's own,
-- verbatim; sink's TestMigration001MatchesAFreshSchema holds the two to the
-- same shape.
--
-- Stop vantage-writer and vantage-api first (both refuse to start against a
-- version they were not built for), apply this, then start the version 2
-- images. Until each collector runs a version 2 image it publishes no
-- heartbeat, and every peer it holds reads `stale` -- still served, with a
-- collector_stale warning -- rather than `up`. Upgrade the collectors in the
-- same rollout.
CREATE TABLE IF NOT EXISTS vantage.collector_beats
(
    collector_id LowCardinality(String),
    started_at   DateTime64(9, 'UTC'),
    beat_at      DateTime64(3, 'UTC'),
    inserted_at  DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(inserted_at)
ORDER BY (collector_id, started_at);

INSERT INTO vantage.schema_version (version)
SELECT 2 WHERE (SELECT max(version) FROM vantage.schema_version) < 2;
