package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// moduleName is the module path prefix; only module-local imports resolve to a
// repository directory, which is what same-package detection needs.
const moduleName = "wtm"

// parsedFile is a parsed non-test file plus facts the boundary check needs: its
// directory, whether it is a _test.go file, and its import bindings (local
// name -> repository directory) so selectors resolve through real imports.
type parsedFile struct {
	dir     string
	ast     *ast.File
	test    bool
	imports map[string]string
	locals  map[string]bool
}

// decl is an exported top-level declaration discovered in the repo.
type decl struct {
	pkg  string
	name string
	dir  string
}

// resolveImports maps each import binding in f to the repository directory of
// the imported package; dot/blank and non-module imports are dropped.
func resolveImports(f *ast.File, root string) map[string]string {
	out := make(map[string]string, len(f.Imports))
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !strings.HasPrefix(path, moduleName+"/") {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			if imp.Name.Name == "." || imp.Name.Name == "_" {
				continue
			}
			name = imp.Name.Name
		}
		out[name] = filepath.Join(root, strings.TrimPrefix(path, moduleName+"/"))
	}
	return out
}

// exportExemption justifies an allowlisted export: reason states why it stays
// exported, refPkgs lists the test-package directory base names that currently
// reference it (verified by TestExportedSymbolsAreShared).
type exportExemption struct {
	reason  string
	refPkgs []string
}

// collectLocals records every declared identifier name in f. A local variable
// that shadows an import name (e.g. var core = ...) must not count as a
// package reference, so the orphan scan treats these names as non-imports.
func collectLocals(f *ast.File) map[string]bool {
	locals := make(map[string]bool)
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			locals[node.Name.Name] = true
		case *ast.TypeSpec:
			locals[node.Name.Name] = true
		case *ast.ValueSpec:
			for _, id := range node.Names {
				locals[id.Name] = true
			}
		case *ast.AssignStmt:
			for _, e := range node.Lhs {
				if id, ok := e.(*ast.Ident); ok {
					locals[id.Name] = true
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{node.Key, node.Value} {
				if id, ok := e.(*ast.Ident); ok {
					locals[id.Name] = true
				}
			}
		case *ast.IncDecStmt:
			if id, ok := node.X.(*ast.Ident); ok {
				locals[id.Name] = true
			}
		}
		return true
	})
	return locals
}

// allowlistedTestOnlyExports are deliberately exported for cross-package tests
// although production code elsewhere never selects them; each entry needs a
// reason and its actual test-package reference set. The testutil package is
// exempt wholesale: it is the cross-package test-support contract by design.
var allowlistedTestOnlyExports = map[string]exportExemption{
	"core.ActiveCapabilities": {
		reason: "pinned by harness capability tests", refPkgs: []string{"harness"},
	},
	"core.AllColumns": {
		reason: "pinned by cli status tests (shared registry contract)", refPkgs: []string{"cli"},
	},
	"core.Applicable": {
		reason: "used by harness/capability_test.go", refPkgs: []string{"harness"},
	},
	"core.BranchLookupMode": {
		reason: "used by cli add/delete tests", refPkgs: []string{"cli"},
	},
	"core.Capabilities": {
		reason: "pinned by harness idempotence and core/cli capability tests", refPkgs: []string{"cli", "core", "harness"},
	},
	"core.ColumnRegistry": {
		reason: "pinned by cli status and core table tests", refPkgs: []string{"cli", "core"},
	},
	"core.ColumnRegistryFor": {
		reason: "used by harness capability tests", refPkgs: []string{"harness"},
	},
	"core.CoreColumns": {
		reason: "pinned by cli status-header and core capability tests", refPkgs: []string{"cli", "core"},
	},
	"core.ErrAmbiguousBranch": {
		reason: "asserted by cli match/add tests", refPkgs: []string{"cli"},
	},
	"core.IntegrationStates": {
		reason: "pinned by harness capability tests", refPkgs: []string{"harness"},
	},
	"core.JSONWorktree": {
		reason: "JSON contract pinned by cli and harness tests", refPkgs: []string{"cli", "harness"},
	},
	"core.LastUsed": {
		reason: "used by harness session tests and core status tests", refPkgs: []string{"core", "harness"},
	},
	"core.MatchLocalBranch": {
		reason: "pinned by cli add tests", refPkgs: []string{"cli"},
	},
	"core.MaxBranchLen": {
		reason: "pinned by cli naming/add tests", refPkgs: []string{"cli"},
	},
	"core.MCPDiscovery": {
		reason: "hook seam type pinned by cli discovery tests", refPkgs: []string{"cli"},
	},
	"core.NormalizeBranch": {
		reason: "pinned by cli naming/add tests", refPkgs: []string{"cli"},
	},
	"core.SessionLastUsed": {
		reason: "used by harness session-mtime tests", refPkgs: []string{"harness"},
	},
	"core.Slugify": {
		reason: "pinned by cli naming tests", refPkgs: []string{"cli"},
	},
	"core.StateWordAbsent": {
		reason: "state-word contract pinned by cli status tests", refPkgs: []string{"cli"},
	},
	"core.StateWordNA": {
		reason: "state-word contract pinned by cli status tests", refPkgs: []string{"cli"},
	},
	"core.StatusColumns": {
		reason: "pinned by cli status-header and core table tests", refPkgs: []string{"cli", "core"},
	},
	"core.UnusedDuration": {
		reason: "used by cli config/gc tests", refPkgs: []string{"cli"},
	},
	"fs.ErrLockBusy": {
		reason: "used by cli temp-process lock tests", refPkgs: []string{"cli"},
	},
	"fs.FileLock": {
		reason: "lock handle used by cli temp-process lock tests", refPkgs: []string{"cli"},
	},
	"fs.AcquireFileLock": {
		reason: "used by cli temp-process lock tests", refPkgs: []string{"cli"},
	},
	"fs.TryAcquireFileLock": {
		reason: "used by cli temp-process lock tests", refPkgs: []string{"cli"},
	},
	"git.RefExists": {
		reason: "used by cli delete and git tests", refPkgs: []string{"cli", "git"},
	},
	"harness.ChunkhoundConfigFile": {
		reason: "pinned by chunkhound/harness tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.ChunkhoundDBDir": {
		reason: "pinned by chunkhound tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.ChunkhoundDBPath": {
		reason: "pinned by chunkhound tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.ClaudeAgent": {
		reason: "claude fixtures used by harness/cli tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.ClaudeSessionSubdir": {
		reason: "session-subdir contract pinned by equivalence + cli discovery tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.MCPConfigFiles": {
		reason: "used by harness MCP and cli tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.PiAgent": {
		reason: "pi fixtures used by harness/cli tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.PiNativeMCPFile": {
		reason: "pinned by pi/harness tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.PiSessionSubdir": {
		reason: "session-subdir contract pinned by equivalence + cli discovery tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.ProjectMCPFile": {
		reason: "pinned by pi/claude MCP tests", refPkgs: []string{"cli", "harness"},
	},
	"harness.RootGuardName": {
		reason: "pinned by harness safety tests", refPkgs: []string{"cli", "harness"},
	},
	"temp.LockPath": {
		reason: "used by cli/temp lock tests", refPkgs: []string{"cli", "temp"},
	},
	"temp.MaxTempTTL": {
		reason: "pinned by cli temp TTL tests", refPkgs: []string{"cli"},
	},
	"temp.TempIdentityPath": {
		reason: "used by cli temp identity tests", refPkgs: []string{"cli"},
	},
	"temp.VerifyTempIdle": {
		reason: "pinned by temp idle/activity tests", refPkgs: []string{"cli", "harness", "temp"},
	},
	"temp.VerifyTempSnapshot": {
		reason: "pinned by cli temp snapshot tests", refPkgs: []string{"cli"},
	},
	"temp.WithLockedTempStore": {
		reason: "used by cli temp tests to mutate the registry", refPkgs: []string{"cli"},
	},
	"ui.ColumnAligns": {
		reason: "used by cli render tests", refPkgs: []string{"cli"},
	},
	"ui.ColumnWidths": {
		reason: "used by cli render and core table tests", refPkgs: []string{"cli", "core"},
	},
	"ui.DispWidth": {
		reason: "used by ui grid/progress and cli status/gc tests", refPkgs: []string{"cli", "ui"},
	},
	"ui.EvNotice": {
		reason: "used by ui progress and harness/cli tests", refPkgs: []string{"cli", "harness", "ui"},
	},
	"ui.EvPhase": {
		reason: "wire-format kind used by ui progress tests", refPkgs: []string{"ui"},
	},
	"ui.EvAdvance": {
		reason: "wire-format kind used by ui progress tests", refPkgs: []string{"ui"},
	},
	"ui.EvDetail": {
		reason: "wire-format kind used by ui progress tests", refPkgs: []string{"ui"},
	},
	"ui.EvDone": {
		reason: "wire-format kind used by ui progress tests", refPkgs: []string{"ui"},
	},
	"ui.GridString": {
		reason: "used by cli render tests", refPkgs: []string{"cli"},
	},
	"ui.LineWidth": {
		reason: "used by cli and core table tests", refPkgs: []string{"cli", "core"},
	},
	"ui.NaturalWidths": {
		reason: "used by core/cli render and ui grid tests", refPkgs: []string{"cli", "core", "ui"},
	},
	"ui.NewProgress": {
		reason: "used by cli progress and harness tests", refPkgs: []string{"cli", "harness"},
	},
	"ui.ProgressEvent": {
		reason: "used by ui progress and harness/cli tests", refPkgs: []string{"cli", "harness", "ui"},
	},
	"ui.RenderCells": {
		reason: "used by cli render tests", refPkgs: []string{"cli"},
	},
	"ui.RenderTable": {
		reason: "used by cli render tests", refPkgs: []string{"cli"},
	},
	"ui.WarnPrefix": {
		reason: "pinned by warning tests", refPkgs: []string{"cli", "harness"},
	},
}

// TestExportedSymbolsAreShared pins the package-boundary rule: every exported
// top-level declaration must be referenced from at least one other package, so
// a symbol used only in its declaring package is unexported (or deleted). Test/
// Benchmark/Example functions and methods are exempt. The allowlist is
// self-checked: every key must resolve to a live exported declaration and must
// still lack production references.
func TestExportedSymbolsAreShared(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	var decls []decl
	var files []parsedFile
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if f.Name.Name == "testutil" {
			return nil
		}
		dir := filepath.Dir(path)
		files = append(files, parsedFile{dir: dir, ast: f, imports: resolveImports(f, root), locals: collectLocals(f)})
		for _, declNode := range f.Decls {
			switch n := declNode.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && n.Name.IsExported() && !skipName.MatchString(n.Name.Name) {
					decls = append(decls, decl{f.Name.Name, n.Name.Name, dir})
				}
			case *ast.GenDecl:
				for _, spec := range n.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							decls = append(decls, decl{f.Name.Name, s.Name.Name, dir})
						}
					case *ast.ValueSpec:
						for _, id := range s.Names {
							if id.IsExported() {
								decls = append(decls, decl{f.Name.Name, id.Name, dir})
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

	var testFiles []parsedFile
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
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if f.Name.Name == "testutil" {
			return nil
		}
		testFiles = append(testFiles, parsedFile{
			dir: filepath.Dir(path), ast: f, test: true,
			imports: resolveImports(f, root), locals: collectLocals(f),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	byKey := map[string]decl{}
	for _, d := range decls {
		byKey[d.pkg+"."+d.name] = d
	}

	var stale []string
	for key, ex := range allowlistedTestOnlyExports {
		d, ok := byKey[key]
		if !ok {
			stale = append(stale, key+" (no such exported declaration)")
			continue
		}
		if !isOrphan(d.name, d.dir, files) {
			stale = append(stale, key+" (already referenced by production)")
			continue
		}
		refs := map[string]bool{}
		for _, tf := range testFiles {
			if tfReferenced(d, tf) {
				refs[filepath.Base(tf.dir)] = true
			}
		}
		if len(refs) == 0 {
			t.Errorf("%s: no test package references it", key)
			continue
		}
		for pkg := range refs {
			if !slices.Contains(ex.refPkgs, pkg) {
				t.Errorf("%s: referenced by test package %q, want subset of %v", key, pkg, ex.refPkgs)
			}
		}
	}
	if len(stale) > 0 {
		t.Errorf("stale allowlist entries:\n  %s", strings.Join(stale, "\n  "))
	}

	var orphans []string
	for _, d := range decls {
		key := d.pkg + "." + d.name
		if _, ok := allowlistedTestOnlyExports[key]; ok {
			continue
		}
		if isOrphan(d.name, d.dir, files) {
			orphans = append(orphans, key)
		}
	}
	if len(orphans) > 0 {
		t.Errorf("exported symbols with no cross-package reference:\n  %s", strings.Join(orphans, "\n  "))
	}
}

// tfReferenced reports whether test file tf references the declaration d.
// Same-directory test files count a direct identifier use; cross-package test
// files count a selector through a module import of d's directory.
func tfReferenced(d decl, tf parsedFile) bool {
	if filepath.Clean(tf.dir) == filepath.Clean(d.dir) {
		return identUsedInTest(tf.ast, d.name)
	}
	referenced := false
	ast.Inspect(tf.ast, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && !tf.locals[x.Name] && tf.imports[x.Name] == filepath.Clean(d.dir) && sel.Sel.Name == d.name {
				referenced = true
				return false
			}
		}
		return true
	})
	return referenced
}

// identUsedInTest reports whether name appears as a direct (unqualified)
// identifier use in f. Selector member names are excluded: a .Sel names a
// member of another package, not a top-level declaration in this one.
func identUsedInTest(f *ast.File, name string) bool {
	found := false
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		if found {
			return false
		}
		switch node := n.(type) {
		case *ast.SelectorExpr:
			ast.Inspect(node.X, visit)
			return false
		case *ast.Ident:
			if node.Name == name {
				found = true
			}
		}
		return !found
	}
	ast.Inspect(f, visit)
	return found
}

// isOrphan reports whether declName (declared in declDir) lacks a production
// reference from another directory: no non-test file outside declDir selects it
// through a real import of declDir. Selectors resolve through import bindings,
// so aliases count and local variables named like the package do not.
func isOrphan(declName, declDir string, files []parsedFile) bool {
	declDir = filepath.Clean(declDir)
	for _, f := range files {
		if f.test || filepath.Clean(f.dir) == declDir {
			continue
		}
		referenced := false
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && !f.locals[x.Name] && f.imports[x.Name] == declDir && sel.Sel.Name == declName {
					referenced = true
					return false
				}
			}
			return true
		})
		if referenced {
			return false
		}
	}
	return true
}
