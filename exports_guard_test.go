package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestExportedSymbolsAreShared pins the package-boundary rule: every exported
// top-level declaration must be referenced from at least one other package.
// The PR's rule is "only shared identifiers become exported"; a symbol that is
// only used inside its declaring package is unexported (or deleted) instead.
// Test/Benchmark/Example functions are exempt (Go requires them exported), and
// methods are exempt (they are reachable through their receiver type, whose own
// export is checked).
func TestExportedSymbolsAreShared(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	type decl struct {
		pkg  string
		name string
	}
	var decls []decl
	byPkg := map[string][]string{}
	var allFiles []string
	skipName := regexp.MustCompile(`^(Test|Benchmark|Example)`)

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		allFiles = append(allFiles, path)
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		pkg := packageName(path)
		if pkg == "" {
			return nil
		}
		byPkg[pkg] = append(byPkg[pkg], path)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declNode := range f.Decls {
			switch d := declNode.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() && !skipName.MatchString(d.Name.Name) {
					decls = append(decls, decl{pkg, d.Name.Name})
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							decls = append(decls, decl{pkg, s.Name.Name})
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								decls = append(decls, decl{pkg, n.Name})
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	refOutside := func(d decl) bool {
		for _, path := range allFiles {
			if d.pkg != "main" && strings.Contains(path, string(os.PathSeparator)+d.pkg+string(os.PathSeparator)) {
				// same package dir; not a cross-package reference
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(d.name) + `\b`).Match(data) {
				return true
			}
		}
		return false
	}

	var orphans []string
	for _, d := range decls {
		if !refOutside(d) {
			orphans = append(orphans, d.pkg+"."+d.name)
		}
	}
	if len(orphans) > 0 {
		t.Errorf("exported symbols with no cross-package reference:\n  %s", strings.Join(orphans, "\n  "))
	}
}

func packageName(path string) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.PackageClauseOnly)
	if err != nil {
		return ""
	}
	return f.Name.Name
}
