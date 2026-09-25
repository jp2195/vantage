# Migrations

One file per version step after the version 1 baseline, named
`NNN-description.sql` and applied in lexical order after `schema.sql`:
`001-collector-beats.sql` moves version 1 to 2. Each ends with a conditional
`INSERT INTO vantage.schema_version SELECT N WHERE (SELECT max(version) FROM vantage.schema_version) < N`,
so applying it twice, or to a database the current `schema.sql` created,
changes nothing. The Helm schema Job applies every file here in order after
`schema.sql`.

## Adding a column to a history table

Seven history tables have a `*_current` table fed by a `SELECT *`
materialized view: `peer_events`, `route_unicast`, `route_vpn`, `route_evpn`,
`ls_nodes`, `ls_links` and `ls_prefixes`. A current table is created `AS`
its history table only on a fresh database, so an existing one gets nothing
from an `ALTER` on the history table. A migration that adds a column to one
of these history tables must, in the same file:

1. Add the same column, with the same type, to its `*_current` table, at
   the same position. Use `AFTER` in both statements, or append the column
   in both.
2. Backfill the column in the current table if its existing rows need a
   value other than the default.

The view needs no change. On ClickHouse 24.8 a `SELECT *` view reads the
history table's columns when each block is inserted and writes them to the
current table by name. A new column reaches the current table once the
current table has it, and until then the view drops it without an error.
After migrating, insert or wait for a new row and check that its value
reached the current table. If it did not, the running version behaves
differently: stop `vantage-writer`, then drop and recreate the view. A row
inserted while the view does not exist reaches history but never its
current table.

Topology reads `SELECT * FROM <t>_current UNION ALL SELECT * FROM <t>`,
which matches columns by position, not by name. If step 1 is skipped,
`/v1/topology` fails with a column-count mismatch. If the column sits at a
different position in each table, the union pairs up the wrong columns: it
fails when their types differ and returns wrong values when they do not.
