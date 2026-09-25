package secret_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// bannedImport reprints what this package exists to hide.
//
// pretty.Compare builds its config with IncludeUnexported: true and sets
// neither PrintStringers nor PrintTextMarshalers, so it walks straight past
// String(), Format() and MarshalText(), reads the unexported field, follows
// the pointer and prints the credential -- see secret.go's package comment,
// which reproduced it against a real sink.Config.
//
// The reason this is a test and not a review note is the module graph:
// prometheus/client_golang's testutil already imports godebug/diff, so the
// module is present. Adding `github.com/kylelemons/godebug/pretty` to a test
// changes no go.mod line and no go.sum line -- there is no diff for code
// review to catch, which is exactly why review will not catch it.
const bannedImport = "github.com/kylelemons/godebug/pretty"

// TestNoGodebugPrettyImport walks every Go file in the module and fails if
// any of them imports the package above.
//
// It scans the whole repository rather than this package, because the risk
// is not here: it is in whichever test one day compares two structs that
// happen to contain a secret.NatsURL or secret.ClickHouseDSN several fields
// down -- sink.Config, a collector config, a flag struct. The author of that
// line has no reason to be thinking about credential redaction at all.
func TestNoGodebugPrettyImport(t *testing.T) {
	root := filepath.Join("..")
	fset := token.NewFileSet()
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip build output and anything version control or tooling
			// owns. testdata is deliberately NOT skipped for being test
			// data -- it holds no Go source, and skipping directories by
			// habit is how a scan like this quietly stops covering things.
			switch d.Name() {
			case ".git", "bin", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			// A file that does not parse cannot be scanned, and silently
			// treating it as clean is how a scanner ends up reporting
			// success over code it never read.
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if p == bannedImport {
				offenders = append(offenders, fset.Position(imp.Pos()).String())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning for %s: %v", bannedImport, err)
	}

	if len(offenders) > 0 {
		t.Fatalf("%s is imported at:\n\t%s\n\n"+
			"That package's Compare reads unexported fields and ignores "+
			"String/Format/MarshalText, so it prints the raw credential out "+
			"of any secret.NatsURL or secret.ClickHouseDSN reachable from "+
			"the values being compared -- however deeply nested. Use "+
			"pretty.Sprint, a hand-written comparison, or compare the "+
			"redacted renderings instead.",
			bannedImport, strings.Join(offenders, "\n\t"))
	}
}
