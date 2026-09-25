package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// flagDefiners are the flag package's methods whose last argument is the
// usage string.
var flagDefiners = map[string]bool{
	"String": true, "StringVar": true, "Int": true, "IntVar": true,
	"Int64": true, "Int64Var": true, "Uint": true, "UintVar": true,
	"Uint64": true, "Uint64Var": true, "Bool": true, "BoolVar": true,
	"Duration": true, "DurationVar": true, "Float64": true, "Float64Var": true,
	"Var": true, "Func": true, "BoolFunc": true, "TextVar": true,
}

// TestFlagUsageHasNoBackquotes pins that no flag usage string under cmd/
// contains a backquote. Package flag reads the first backquoted word in a
// usage string as the flag's value name, so "`query ls nodes`" in a usage
// made -h print "-local-node query ls nodes" instead of "-local-node string".
func TestFlagUsageHasNoBackquotes(t *testing.T) {
	files, err := filepath.Glob("../*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !flagDefiners[sel.Sel.Name] {
				return true
			}
			usage, ok := constString(call.Args[len(call.Args)-1])
			if !ok {
				return true
			}
			checked++
			if strings.Contains(usage, "`") {
				t.Errorf("%s: flag usage contains a backquote, which -h prints as the value name: %q",
					fset.Position(call.Pos()), usage)
			}
			return true
		})
	}
	if checked < 50 {
		t.Fatalf("checked only %d flag usage strings; the scan is not finding the flag definitions", checked)
	}
}

// constString folds a string literal, or a + concatenation of them.
func constString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := constString(v.X)
		if !ok {
			return "", false
		}
		r, ok := constString(v.Y)
		return l + r, ok
	case *ast.ParenExpr:
		return constString(v.X)
	}
	return "", false
}
