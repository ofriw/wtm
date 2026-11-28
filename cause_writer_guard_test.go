package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// causeWriterRule pins one estimate cause to its single permitted writer. The
// invariant: the cause may be passed to an addEstimated*/priceClass* sink only
// by its owning function, so a stray writer cannot stamp an assumption caveat
// onto rows that never earned it.
type causeWriterRule struct {
	cause string // identifier for the assumed price
	file  string // base name of the file the write must live in
	fn    string // enclosing function name
}

var causeWriterRules = []causeWriterRule{
	{"causeSubscriptionOverage", "usage.go", "estimatedZero"},
	{"causeCacheWrite", "rates.go", "addEstimatedClass"},
}

// isEstimateCaller reports whether a callee name is an estimate sink:
// addEstimated* (the money method and the class variant) and priceClass are
// the only functions that may receive a cause that marks a price as assumed.
func isEstimateCaller(name string) bool {
	return strings.HasPrefix(name, "addEstimated") || strings.HasPrefix(name, "priceClass")
}

// calleeName resolves a call's function to its bare name, whether a free
// function (Ident) or a method (SelectorExpr).
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// TestCauseWriterGuard enforces the single-writer invariants with the parser,
// replacing the Makefile literal-count greps. Literal counts coupled to layout
// and missed indirection; the AST names the exact file and line of an
// offending use, so drift is loud. Test files are excluded: they legitimately
// construct prices with these causes.
func TestCauseWriterGuard(t *testing.T) {
	fset := token.NewFileSet()
	for _, rule := range causeWriterRules {
		var violations []string
		err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || !isEstimateCaller(calleeName(call)) {
						return true
					}
					for _, arg := range call.Args {
						id, ok := arg.(*ast.Ident)
						if !ok || id.Name != rule.cause {
							continue
						}
						if fd.Name.Name != rule.fn || filepath.Base(path) != rule.file {
							violations = append(violations,
								fmt.Sprintf("%s:%d: %s passed to %s inside %s (want %s in %s)",
									path, fset.Position(id.Pos()).Line,
									rule.cause, calleeName(call), fd.Name.Name,
									rule.fn, rule.file))
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(violations) > 0 {
			t.Errorf("%s single-writer invariant violated:\n%s", rule.cause, strings.Join(violations, "\n"))
		}
	}
}
