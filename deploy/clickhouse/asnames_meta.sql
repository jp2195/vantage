-- The AS holder name dataset's own publication date, carried into
-- ClickHouse as a companion, one-row dictionary beside asnames.sql -- so
-- GET /v1/asnames can say WHEN RIPE published the names it is serving,
-- not merely when a person last ran `make fetch-asnames` against them.
--
-- WHY A SEPARATE DICTIONARY, NOT A ROW INSIDE asnames ITSELF
--
-- Three shapes were considered and rejected in favor of this one:
--
--   - Mounting deploy/dev/asnames/asn.meta (the human-readable two-line
--     sidecar fetch-asnames already writes) into the API container: new
--     plumbing in BOTH docker-compose.dev.yml and Helm for one string, and
--     Helm's data path is already the awkward part of this whole feature
--     (see that chart's own README section).
--   - system.dictionaries.last_successful_update_time: that is when THIS
--     ClickHouse server LOADED the dictionary, not when RIPE PUBLISHED the
--     data -- the wrong fact entirely, since staleness of the DATA is the
--     one thing this date exists to let a caller judge.
--   - A sentinel row at asn = 0 inside vantage.asnames itself: AS 0 is
--     reserved (RFC 7607) but still a lookupable key, so a sentinel there
--     would pollute a NAMES dictionary with a row that is not a name.
--
-- A companion dictionary makes the date travel WITH the data by
-- construction (both come from the same `make fetch-asnames` run, applied
-- together the same way), reuses the FILE-source-plus-status-check
-- plumbing asnames.sql and query/asnames.go already established; this was
-- verified twice against real containers, and needs no new mount.
--
-- WHY THIS IS NOT A MIGRATION, AND WHY IT IS OPTIONAL
--
-- Both reasons are asnames.sql's own, verbatim: this creates no table, adds
-- no column, and gates no daemon's startup, so it stays outside the
-- schema_version sequence in migrations/ entirely; and ClickHouse's
-- dictionaries_lazy_load (default true, confirmed in this image's
-- config.xml) means CREATE/ATTACH DICTIONARY never reads the source file
-- at server startup, only on first use -- so a stack with no
-- asnames_meta.tsv yet attaches this object with nothing behind it
-- (status NOT_LOADED) rather than failing to start.
--
-- query.(*Q).ASNamesPublished calls dictGetOrDefault on THIS dictionary
-- unconditionally, then classifies its own error rather than checking
-- system.dictionaries.status first -- for the identical reason asnames.sql's
-- header gives for asnames itself, and the identical fix: a missing
-- object raises Code 36 BAD_ARGUMENTS and a missing source file
-- raises Code 107 FILE_DOESNT_EXIST, neither is the default value
-- dictGetOrDefault would otherwise return, and BOTH are exactly what a
-- ClickHouse restart leaves this dictionary as (status NOT_LOADED, data
-- and all) -- so calling unconditionally, rather than gating on a status
-- check first, is what lets the first real request after a restart supply
-- ClickHouse's own "first use" and self-heal with no human step. See
-- query/asnames.go's own isNoASNamesDataset doc comment and
-- the same package's tests for the measurements.
--
-- CREATE DATABASE IF NOT EXISTS is redundant given
-- docker-compose.dev.yml's CLICKHOUSE_DB=vantage (see asnames.sql's own
-- header for the confirmed startup order) and stays anyway as the same
-- cheap, idempotent guard that file keeps, for whoever applies this file
-- by hand or on its own.
CREATE DATABASE IF NOT EXISTS vantage;

-- The path is absolute for the identical reason asnames.sql's own path is:
-- ClickHouse 24.8.14's FILE dictionary source rejects a relative path
-- (Code 481, measured against this exact image) even though the `file()`
-- table function resolves the same relative text correctly. This resolves
-- inside user_files_path via docker-compose.dev.yml's existing bind of
-- deploy/dev/asnames at user_files/asnames -- the SAME mount asnames.sql
-- already reads asn.tsv through, so no new mount is needed for this file
-- either.
--
-- id is a fixed sentinel, not an AS number: this dictionary holds exactly
-- one row, and 0 was picked because it raises no AS-0-is-reserved question
-- here the way it would inside vantage.asnames -- this is not that
-- dictionary, and this key is not an ASN.
--
-- LAYOUT(HASHED()) and LIFETIME(0) match asnames.sql's own choices, for the
-- same reasons: one row is trivially small for a hash layout, and the file
-- changes only when a person re-runs `make fetch-asnames`, which is a
-- human cadence a background reload timer would only be polling for no
-- reason. Picking up a re-fetched file is `SYSTEM RELOAD DICTIONARY
-- vantage.asnames_meta`, run by that same person, right after they reload
-- vantage.asnames itself.
CREATE DICTIONARY IF NOT EXISTS vantage.asnames_meta
(
    id            UInt8,
    last_modified String
)
PRIMARY KEY id
SOURCE(FILE(path '/var/lib/clickhouse/user_files/asnames/asnames_meta.tsv' format 'TSV'))
LAYOUT(HASHED())
LIFETIME(0);

-- APPLYING IT
--
-- Same convention as asnames.sql and migrations/: the dev stack's
-- docker-entrypoint-initdb.d only runs against an empty data directory, so
-- this file reaches a fresh stack automatically but a stack that already
-- has data needs it applied by hand:
--
--   docker exec -i vantage-clickhouse-1 clickhouse-client --multiquery \
--     < deploy/clickhouse/asnames_meta.sql
--
-- Safe to run against a stack that already has this dictionary (IF NOT
-- EXISTS on both statements) and safe to run against a stack with no
-- deploy/dev/asnames/asnames_meta.tsv at all, for the same reason
-- asnames.sql's own "WHY IT IS OPTIONAL" section gives.
