-- The AS holder name dictionary: attaches "Level 3 Parent, LLC" to AS3356
-- wherever an AS number is shown, without touching schema_version.
--
-- WHY THIS IS NOT A MIGRATION
--
-- Every file in migrations/ moves the archive from one schema_version to
-- the next, and vantage-writer/vantage-api refuse to start against a
-- version they were not built for (see schema.sql's own comment on
-- ExpectedSchemaVersion). A dictionary creates no table, holds no rows a
-- writer inserts into, and nothing downstream needs a version bump to read
-- it -- gating two daemons' startup on an optional lookup would make the
-- lookup a required part of every future schema change. So this lives
-- beside schema.sql, outside the version sequence entirely.
--
-- WHY IT IS OPTIONAL, ON PURPOSE
--
-- `make fetch-asnames` writes deploy/dev/asnames/asn.tsv, and that
-- directory is gitignored -- a fresh clone has no file there until someone
-- runs the fetch. ClickHouse's dictionaries_lazy_load defaults to true
-- (confirmed in this image's config.xml), so CREATE/ATTACH DICTIONARY never
-- reads the source file at server startup; it reads it on first use. That
-- is what keeps a missing file from being a startup problem at all -- the
-- dictionary attaches as an object with no rows behind it yet, and stays
-- that way (status NOT_LOADED in system.dictionaries) until something calls
-- dictGet on it. What a missing file does NOT do is make dictGetOrDefault
-- return the default: measured against this exact file (present, then
-- removed), calling it raises Code 107 FILE_DOESNT_EXIST when the file is
-- gone, and Code 36 BAD_ARGUMENTS if the dictionary object itself was never
-- created (e.g. this file was never applied to an older stack) -- neither
-- is the default value. query/asnames.go's own dictGetOrDefault calls
-- classify these two exact codes as "no dataset" rather than surfacing
-- them as errors, and the reason those calls can run
-- UNCONDITIONALLY, on every request, with no separate status check first.
-- That matters beyond just this file's own optionality: it is what makes
-- dictionaries_lazy_load's own "loads on first use" contract actually
-- self-heal this dictionary after a ClickHouse restart, which resets
-- EVERY dictionary back to this same NOT_LOADED state regardless of
-- whether its data was ever loaded before -- the first real request after
-- a restart is itself the "first use," and it succeeds if the file is
-- there. See query/asnames.go's own isNoASNamesDataset doc comment and
-- the same package's tests for the measurements; no human step is needed
-- for a restart. A genuine re-fetch (a NEW `make fetch-asnames` run,
-- overwriting an already-LOADED dictionary's file) is different --
-- LIFETIME(0) below means that still needs an explicit SYSTEM RELOAD
-- DICTIONARY, on purpose; see README.md's "AS holder names" section.
--
-- CREATE DATABASE is redundant given docker-compose.dev.yml's CLICKHOUSE_DB=
-- vantage -- the image's own /entrypoint.sh runs `CREATE DATABASE IF NOT
-- EXISTS $CLICKHOUSE_DB` itself, unconditionally, before it ever touches
-- docker-entrypoint-initdb.d/*. (Checked: deleting this line and booting a
-- fresh volume works fine -- all 12 objects present, no failure of any
-- kind.) It stays anyway as a cheap, idempotent guard against the one thing
-- this file cannot control: whoever applies it by hand, or a future stack
-- that runs this file without CLICKHOUSE_DB set.
CREATE DATABASE IF NOT EXISTS vantage;

-- The path is absolute, not relative to user_files_path the way the `file()`
-- table function's docs describe -- measured, not stylistic. ClickHouse
-- 24.8.14's FILE dictionary source checks a relative path for containment
-- inside user_files_path by comparing the RAW relative string against the
-- absolute user_files_path, which can never match, so a relative path here
-- fails every load with "File path asnames/asn.tsv is not inside
-- /var/lib/clickhouse/user_files/" (Code 481). `file()` the table function
-- resolves the same relative path correctly; the dictionary source does
-- not. The absolute path below is inside user_files_path (still enforced --
-- an absolute path pointing outside it is still rejected) and loads fine.
-- docker-compose.dev.yml mounts deploy/dev/asnames at user_files/asnames,
-- so this resolves to the TSV `make fetch-asnames` wrote -- three
-- tab-separated columns already parsed out of RIPE's "ASN name, CC" line
-- format; that parse is fetch-asnames's job, not this file's.
--
-- LAYOUT(HASHED()): 122,442 rows at 20 MiB resident, measured -- small
-- enough that a hash table beats worrying about a leaner layout.
--
-- LIFETIME(0) disables ClickHouse's periodic background reload. The file
-- changes only when a person re-runs `make fetch-asnames`, so a refresh
-- interval would be a timer polling a file that changes on a human
-- cadence, not a machine one. Picking up a re-fetched file is `SYSTEM
-- RELOAD DICTIONARY vantage.asnames`, run by the same person.
CREATE DICTIONARY IF NOT EXISTS vantage.asnames
(
    asn     UInt32,
    name    String,
    country String
)
PRIMARY KEY asn
SOURCE(FILE(path '/var/lib/clickhouse/user_files/asnames/asn.tsv' format 'TSV'))
LAYOUT(HASHED())
LIFETIME(0);

-- APPLYING IT
--
-- The dev stack's docker-entrypoint-initdb.d only runs against an empty
-- data directory (see README.md's "Applying the schema"), so this file
-- reaches a fresh stack automatically but a stack that already has data
-- needs it applied by hand, same convention as migrations/:
--
--   docker exec -i vantage-clickhouse-1 clickhouse-client --multiquery \
--     < deploy/clickhouse/asnames.sql
--
-- Safe to run against a stack that already has the dictionary -- both
-- statements are IF NOT EXISTS. Safe to run against a stack with no
-- deploy/dev/asnames/asn.tsv at all -- see WHY IT IS OPTIONAL above.
