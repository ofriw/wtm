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

// allowlistedTestOnlyExports are deliberately exported for cross-package tests
// although production code elsewhere never selects them; each entry needs a
// reason. The testutil package is exempt wholesale: it is the cross-package
// test-support contract by design.
var allowlistedTestOnlyExports = map[string]string{
	"core.ActiveCapabilities":      "pinned by cli/status and gc tests",
	"core.AllColumns":              "pinned by cli/schema tests (shared registry contract)",
	"core.Applicable":              "used by core's own table tests and cli golden paths",
	"core.BranchLookupMode":        "used by add/delete tests",
	"core.Capabilities":            "pinned by harness idempotence tests",
	"core.ColumnRegistry":          "pinned by core table tests",
	"core.ColumnRegistryFor":       "used by core capability tests",
	"core.CoreColumns":             "pinned by status-header tests",
	"core.ErrAmbiguousBranch":      "asserted by match/add tests",
	"core.IntegrationStates":       "pinned by JSON tests",
	"core.JSONWorktree":            "JSON contract pinned by cli tests",
	"core.LastUsed":                "used by harness session tests",
	"core.MatchLocalBranch":        "pinned by add tests",
	"core.MaxBranchLen":            "pinned by naming/add tests",
	"core.MCPDiscovery":            "hook seam type exercised by core/cli tests",
	"core.NormalizeBranch":         "pinned by naming/add tests",
	"core.SessionLastUsed":         "used by harness session tests",
	"core.Slugify":                 "pinned by naming tests",
	"core.StateWordAbsent":         "state-word contract pinned by status tests",
	"core.StateWordNA":             "state-word contract pinned by status tests",
	"core.StatusColumns":           "pinned by status-header tests",
	"core.UnusedDuration":          "used by cli/gc tests",
	"fs.UnlockFile":                "windows/unix lock test pair",
	"git.RefExists":                "used by cli/delete tests",
	"harness.ChunkhoundConfigFile": "pinned by chunkhound/harness tests",
	"harness.ChunkhoundDBDir":      "pinned by chunkhound tests",
	"harness.ChunkhoundDBPath":     "pinned by chunkhound tests",
	"harness.ClaudeAgent":          "claude fixtures used by harness/cli tests",
	"harness.ClaudeSessionSubdir":  "session-subdir contract pinned by equivalence + cli discovery tests",
	"harness.MCPConfigFiles":       "used by harness MCP tests",
	"harness.PiAgent":              "pi fixtures used by harness/cli tests",
	"harness.PiNativeMCPFile":      "pinned by pi tests",
	"harness.PiSessionSubdir":      "session-subdir contract pinned by equivalence + cli discovery tests",
	"harness.ProjectMCPFile":       "pinned by pi/claude MCP tests",
	"harness.RootGuardName":        "pinned by harness safety tests",
	"temp.LockPath":                "used by temp/cli lock tests",
	"temp.MaxTempTTL":              "pinned by temp TTL tests",
	"temp.TempIdentityPath":        "used by temp identity tests",
	"temp.VerifyTempIdle":          "pinned by temp idle tests",
	"temp.VerifyTempSnapshot":      "pinned by temp removal tests",
	"temp.WithLockedTempStore":     "used by cli temp tests to mutate the registry",
	"ui.ColumnAligns":              "used by core/cli render tests",
	"ui.ColumnWidths":              "used by core/table tests",
	"ui.DispWidth":                 "used by ui tests",
	"ui.EvNotice":                  "used by ui progress tests",
	"ui.GridString":                "used by cli render tests",
	"ui.LineWidth":                 "used by core/table and gc tests",
	"ui.NaturalWidths":             "used by core/cli render tests",
	"ui.NewProgress":               "used by cli progress tests",
	"ui.ProgressEvent":             "used by ui progress tests",
	"ui.RenderCells":               "used by cli render tests",
	"ui.RenderTable":               "used by cli render tests",
	"ui.WarnPrefix":                "pinned by warning tests",
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
		files = append(files, parsedFile{dir: dir, ast: f, imports: resolveImports(f, root)})
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

	byKey := map[string]decl{}
	for _, d := range decls {
		byKey[d.pkg+"."+d.name] = d
	}

	var stale []string
	for key := range allowlistedTestOnlyExports {
		d, ok := byKey[key]
		if !ok {
			stale = append(stale, key+" (no such exported declaration)")
			continue
		}
		if !isOrphan(d.name, d.dir, files) {
			stale = append(stale, key+" (already referenced by production)")
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
				if x, ok := sel.X.(*ast.Ident); ok && f.imports[x.Name] == declDir && sel.Sel.Name == declName {
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
