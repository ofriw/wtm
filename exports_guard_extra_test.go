package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func mustParse(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const declDir = "/src/internal/core"

// TestReferencedOutsideRegression pins the boundary rules the live guard
// previously violated. isOrphan returns true for an orphan (no production
// cross-directory reference) and false when the symbol is shared.
func TestReferencedOutsideRegression(t *testing.T) {
	commentOnly := parsedFile{dir: "/src/a", ast: mustParse(t, "package a\n// mentions core.Bar only in prose\n")}
	prodSrc := mustParse(t, "package b\nimport domain \"wtm/internal/core\"\nvar x = domain.Bar\n")
	prodSelector := parsedFile{dir: "/src/b", ast: prodSrc, imports: resolveImports(prodSrc, "/src")}
	testSelector := parsedFile{dir: "/src/c", ast: mustParse(t, "package c\nimport \"wtm/internal/core\"\nvar y = core.Bar\n"), test: true}

	// Comment-only mention is not a reference: orphan.
	if !isOrphan("Bar", declDir, []parsedFile{commentOnly}) {
		t.Error("comment-only mention counted as a reference")
	}
	// Real selector through an import of the declaring directory is a
	// reference, even when the package binding is aliased.
	if isOrphan("Bar", declDir, []parsedFile{prodSelector}) {
		t.Error("real selector in a different dir did not count")
	}
	// Same directory = same package: not a cross-package reference.
	sameDir := parsedFile{dir: declDir, ast: mustParse(t, "package core\nvar x = core.Bar\n"), imports: map[string]string{"core": declDir}}
	if !isOrphan("Bar", declDir, []parsedFile{sameDir}) {
		t.Error("same-directory selector counted as cross-package reference")
	}
	// Test-only files never count as production references.
	if !isOrphan("Bar", declDir, []parsedFile{testSelector}) {
		t.Error("test-only file counted as a production reference")
	}

	// Main has no same-package exemption: own-dir use is still an orphan.
	mainOnly := parsedFile{dir: "/repo", ast: mustParse(t, "package main\nfunc main() { _ = main.Foo }\n")}
	if !isOrphan("Foo", "/repo", []parsedFile{mainOnly}) {
		t.Error("main symbol used only in its own directory counted as shared")
	}

	// Alias resolving to a different package directory is not a reference
	// to the declared package.
	otherSrc := mustParse(t, "package cli\nimport domain \"wtm/internal/ui\"\nvar x = domain.Bar\n")
	otherProd := parsedFile{dir: "/repo/internal/cli", ast: otherSrc, imports: resolveImports(otherSrc, "/repo")}
	if !isOrphan("Bar", declDir, []parsedFile{otherProd}) {
		t.Error("alias to another package counted as a reference")
	}
	// Local variable named like the package cannot hide an orphan: without a
	// module import of the declaring directory the selector is not a reference.
	shadowSrc := mustParse(t, "package cli\nvar core = struct{ Bar int }{}\nvar x = core.Bar\n")
	shadowProd := parsedFile{dir: "/repo/internal/cli", ast: shadowSrc, imports: resolveImports(shadowSrc, "/repo")}
	if !isOrphan("Bar", declDir, []parsedFile{shadowProd}) {
		t.Error("local shadowing counted as a cross-package reference")
	}
}

// TestShadowedImportNameNotPackageRef pins that a top-level local var that
// shadows an import name stops the selector from counting as a package ref,
// even when the import of the declaring directory resolves.
func TestShadowedImportNameNotPackageRef(t *testing.T) {
	shadowSrc := mustParse(t, "package cli\nimport \"wtm/internal/core\"\nvar core = struct{ Bar int }{}\nvar x = core.Bar\n")
	shadowProd := parsedFile{dir: "/repo/internal/cli", ast: shadowSrc, imports: resolveImports(shadowSrc, "/src"), locals: collectLocals(shadowSrc)}
	if !isOrphan("Bar", declDir, []parsedFile{shadowProd}) {
		t.Error("top-level local var shadowing an import name counted as a package reference")
	}
}
