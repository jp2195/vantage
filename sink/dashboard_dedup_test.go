package sink

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Every route and event table is ReplacingMergeTree, because a redelivered
// JetStream envelope writes a byte-identical row that the merge collapses
// later (see TestRowsForRedeliveryIsByteIdentical). "Later" is the operative
// word: until that merge runs, the duplicate is a real second row, and
// whether a query sees it depends entirely on how the query is written.
//
// A scale test on 2026-08-13 measured both halves of that. Dropping FINAL is
// a large win -- 3,740,000 rows read versus 77,870, 182 MiB versus
// 146.50 KiB, 48x the rows and 1,270x the memory -- AND it is safe only for
// latest-value aggregates: argMax, max and any return the same answer
// whether or not the duplicate has been merged away. count(), sum() and
// avg() do not. They count the duplicate.
//
// That split is why this test exists rather than a blanket rule: the
// dashboards are where the finding was measured and the one place it was
// never applied, so the FINAL removals still to come need something that
// tells the safe removals from the unsafe ones.
//
// WHERE THE BEHAVIORAL SUITE ALREADY DOES THAT, IT DOES IT WELL, and this
// test is not a substitute for it. Three fixtures carry a deliberate
// unmerged duplicate -- insertEvpnChurnFixture and insertRouteChurnFixture
// each redeliver a genuine re-advertisement, and insertParseAnomalyFixture
// inserts the same row twice at one stream_seq for exactly this purpose.
// Stripping every FINAL from all thirteen dashboards fails 12 tests,
// 11 of them behavioral.
//
// THE GAP IS THE DASHBOARDS WHOSE FIXTURES CARRY NO DUPLICATE. Stripping
// all 19 FINAL occurrences from fleet-health alone fails exactly one test:
// this one. Nothing in that fixture is duplicated, so no behavioral
// assertion can move. That is what this guards, and the whole of what it
// guards.
//
// LIMITS, stated because they bound what a pass here means. This reads the
// target as one string: a target that counts raw rows in one subquery while
// using uniqExact in another reads as deduped and is not flagged. Judging
// which table an aggregate actually applies to needs a real SQL parser, and
// the stricter per-table rule was tried and rejected -- it flagged six
// targets that were all correct, mostly because a table NAME appears as a
// string literal ('route_unicast' AS src) beside a genuinely FINAL read.
// This is a ratchet against the dangerous edit, not a proof of correctness.
func TestEveryCountingDashboardTargetDedups(t *testing.T) {
	replacing := replacingMergeTreeTables(t)
	if len(replacing) < 5 {
		t.Fatalf("derived only %d ReplacingMergeTree tables from schema.sql (%v); "+
			"the derivation is wrong and this test would pass by examining nothing",
			len(replacing), replacing)
	}

	// Aggregates whose value changes when one row becomes two. The list is
	// wider than what the dashboards use today on purpose -- evpn-churn
	// already reaches for quantileIf, which a narrower regex would miss,
	// leaving that target covered only because it also uses countIf. A
	// future panel reaching for quantile, median, topK, a standard deviation
	// or groupArray alone would have slipped through. Deliberately absent:
	// min, max, any, argMin, argMax and every uniq* form, which are the
	// duplication-INSENSITIVE aggregates FINAL can safely be dropped around
	// (see the measurement at the top of this file).
	dupSensitive := regexp.MustCompile(`(?i)\b(count|countIf|sum|sumIf|avg|avgIf|` +
		`quantile\w*|median\w*|topK\w*|stddev\w*|var(Pop|Samp)\w*|groupArray\w*)\s*\(`)
	// Constructs that collapse the duplicate before it can be counted.
	dedups := regexp.MustCompile(`(?i)\bFINAL\b|\buniqExact\s*\(|\buniq\s*\(|\bargMax\s*\(|\bDISTINCT\b`)

	var examined int
	for _, dash := range allDashboards {
		for _, tg := range dashboardTargets(t, dash) {
			// String literals first: parse-anomalies labels each arm of a
			// UNION with the table's own name ('route_unicast' AS src)
			// right beside a real, FINAL-qualified read of it. Matching
			// those as table reads is what made the stricter rule useless.
			sql := stripSQLStringLiterals(tg.rawSQL)
			if !readsAnyOf(sql, replacing) || !dupSensitive.MatchString(sql) {
				continue
			}
			examined++
			if !dedups.MatchString(sql) {
				t.Errorf("%s panel %q target %q counts rows from a "+
					"ReplacingMergeTree table with no dedup. A redelivered "+
					"envelope is a real second row until the merge runs, so "+
					"this panel reports a number that is too high for as long "+
					"as that takes. Keep FINAL here, or dedup explicitly with "+
					"uniqExact/argMax -- and weigh the cost before "+
					"choosing: FINAL costs 48x the rows read.",
					tg.dashboard, tg.panel, tg.refID)
			}
		}
	}
	// The failure mode of a discovery-based check is discovering zero.
	//
	// THIS FLOOR IS EXPECTED TO FALL, and falling is progress rather than
	// regression: a target converted from count() to uniqExact leaves this
	// population deliberately, because it no longer needs the dedup this test
	// polices. It went 20 -> 16 on 2026-09-19 when parse-anomalies' four
	// panels were converted to count MESSAGES, uniqExact((stream, stream_seq)),
	// instead of rows. Lower it when that happens and say which targets left;
	// do NOT lower it to make an unexplained drop go green, which is the one
	// thing this guard exists to catch.
	if examined < 16 {
		t.Fatalf("examined %d targets that both read a ReplacingMergeTree table "+
			"and use a duplication-sensitive aggregate, want at least 16 -- "+
			"the walk is not finding what ships", examined)
	}
}

// replacingMergeTreeTables reads the engine of every table out of schema.sql
// rather than restating a list here: a table that becomes ReplacingMergeTree
// later is covered the day it does, with nothing to remember to update.
func replacingMergeTreeTables(t *testing.T) []string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	re := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS vantage\.(\w+).*?ENGINE\s*=\s*(\w+)`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(ddl), -1) {
		if m[2] == "ReplacingMergeTree" {
			out = append(out, m[1])
		}
	}
	return out
}

func readsAnyOf(sql string, tables []string) bool {
	for _, tb := range tables {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(tb) + `\b`).MatchString(sql) {
			return true
		}
	}
	return false
}

// stripSQLStringLiterals blanks single-quoted literals so a table name used
// as a label is not mistaken for a read of that table.
func stripSQLStringLiterals(sql string) string {
	return regexp.MustCompile(`'[^']*'`).ReplaceAllString(sql, "''")
}
