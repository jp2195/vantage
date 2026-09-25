package query

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestShippedDictionaryDDLDeclaresEveryAttributeTheQueriesAsk closes a hole
// that nothing else in this repository can see, and that would stay open
// forever once opened.
//
// THE FAILURE. Rename an attribute in the shipped DDL (or in asNamesSQL)
// without renaming it in the other, and ClickHouse answers a dictGet
// against a FULLY LOADED dictionary with:
//
//	Code: 36. DB::Exception: No such attribute 'nmae' ... (BAD_ARGUMENTS)
//
// Measured directly against this project's own dev stack, with
// vantage.asnames LOADED and returning the real holder name for the
// correct attribute name in the same session. Code 36 is exactly what
// isNoASNamesDataset (query/asnames.go) classifies as "there is no
// dataset" -- correctly, since a missing dictionary OBJECT raises the same
// code -- so an attribute-name drift does not surface as an error. It
// surfaces as GET /v1/asnames reporting asnames_loaded: false, every
// screen rendering bare ASNs and saying the dataset is not loaded, on a
// stack where it IS loaded. Silently, permanently, and with the daemon's
// own degradation path making it look intentional.
//
// WHY NOTHING ELSE CATCHES IT. Keeping both dictionaries out of the
// schema_version sequence is right, for the reasons asnames.sql's header
// gives, but it also means no migration test walks this DDL. The live
// tests in query/asnames_test.go build their own CLICKHOUSE-sourced
// fixture dictionaries rather than applying the shipped files (they run on
// the host; a FILE source resolves inside the server's container), so they
// exercise these same statements against a DDL this test is the only thing
// comparing them to.
//
// WHAT THIS ASSERTS. The attribute names are derived from the Go constants
// by parsing their dictGetOrDefault calls, not typed in here -- so adding
// a fourth attribute to a query automatically requires the shipped DDL to
// declare it, rather than requiring someone to remember this file. Both
// shipped copies are checked: deploy/clickhouse/ is what a person applies
// by hand and what docker-compose.dev.yml mounts, and the chart's own
// files/dictionaries/ copy is what reaches Kubernetes through
// job-schema.yaml. A drift in either produces the identical silent lie.
func TestShippedDictionaryDDLDeclaresEveryAttributeTheQueriesAsk(t *testing.T) {
	asked := attributesAskedFor(t, asNamesSQL, asNamesPublishedSQL)

	// Anti-vacuity. Every assertion below iterates `asked`, so a regexp
	// that quietly stopped matching would turn this whole test into a
	// no-op that still prints ok -- the exact shape of failure this
	// test exists to catch. These three are what the queries ask
	// for today; the loop above is what keeps a fourth from escaping.
	for _, want := range []dictAttr{
		{"asnames", "name"},
		{"asnames", "country"},
		{"asnames_meta", "last_modified"},
	} {
		if !asked[want] {
			t.Fatalf("parsed no %q attribute for dictionary %q out of the query constants; "+
				"the parse below is broken, not the DDL -- every other assertion in this "+
				"test iterates that parse and would pass vacuously. Got: %v",
				want.attr, want.dict, sortedAttrs(asked))
		}
	}

	for _, dir := range []string{
		filepath.Join("..", "deploy", "clickhouse"),
		filepath.Join("..", "deploy", "helm", "vantage", "files", "dictionaries"),
	} {
		declared := map[dictAttr]bool{}
		for _, f := range []string{"asnames.sql", "asnames_meta.sql"} {
			path := filepath.Join(dir, f)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read shipped DDL %s: %v", path, err)
			}
			for da := range declaredAttributes(t, path, string(b)) {
				declared[da] = true
			}
		}
		if len(declared) == 0 {
			t.Fatalf("%s: parsed no CREATE DICTIONARY attributes at all; the DDL parse is "+
				"broken and the assertion below would pass vacuously", dir)
		}
		for want := range asked {
			if !declared[want] {
				t.Errorf("%s: the queries call dictGetOrDefault for attribute %q on "+
					"dictionary %q, but the shipped DDL there declares no such attribute "+
					"(it declares: %v).\n\nThis does NOT fail loudly at runtime: ClickHouse "+
					"answers Code 36 BAD_ARGUMENTS, isNoASNamesDataset classifies that as "+
					"\"no dataset\", and every screen reports the names as not loaded on a "+
					"stack where they are. Rename it in both places or neither.",
					dir, want.attr, want.dict, sortedAttrs(declared))
			}
		}
	}
}

// dictAttr is one (dictionary, attribute) pair -- the unit that has to
// agree, since the same attribute name on the wrong dictionary is just as
// broken as a misspelled one.
type dictAttr struct{ dict, attr string }

// dictGetCall matches a dictGetOrDefault call in this package's SQL
// constants, capturing the dictionary's unqualified name and the attribute
// it asks for. The constants name their database as the `%[1]s` verb that
// (*Q).db fills in, so that prefix is matched literally rather than as an
// identifier.
var dictGetCall = regexp.MustCompile(`dictGetOrDefault\(\s*'%\[1\]s\.(\w+)'\s*,\s*'(\w+)'`)

func attributesAskedFor(t *testing.T, sqls ...string) map[dictAttr]bool {
	t.Helper()
	out := map[dictAttr]bool{}
	for _, sql := range sqls {
		for _, m := range dictGetCall.FindAllStringSubmatch(sql, -1) {
			out[dictAttr{dict: m[1], attr: m[2]}] = true
		}
	}
	return out
}

// createDictionary matches one CREATE DICTIONARY statement's name and its
// parenthesized attribute list, stopping at PRIMARY KEY so that SOURCE()
// and LAYOUT()'s own parentheses cannot be swallowed into the body.
var createDictionary = regexp.MustCompile(`(?s)CREATE DICTIONARY\s+(?:IF NOT EXISTS\s+)?\w+\.(\w+)\s*\((.*?)\)\s*PRIMARY KEY`)

func declaredAttributes(t *testing.T, path, ddl string) map[dictAttr]bool {
	t.Helper()
	out := map[dictAttr]bool{}
	for _, m := range createDictionary.FindAllStringSubmatch(ddl, -1) {
		dict := m[1]
		for _, line := range strings.Split(m[2], "\n") {
			// `asn     UInt32,` -> `asn`. Comment lines and the blank
			// ones around them carry no leading identifier and drop out.
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "--") {
				continue
			}
			if name, _, ok := strings.Cut(line, " "); ok {
				out[dictAttr{dict: dict, attr: strings.TrimSuffix(name, ",")}] = true
			}
		}
	}
	if len(out) == 0 {
		t.Errorf("%s: matched no CREATE DICTIONARY body", path)
	}
	return out
}

func sortedAttrs(m map[dictAttr]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.dict+"."+k.attr)
	}
	// A stable order so a failure message reads the same twice.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
