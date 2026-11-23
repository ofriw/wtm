package core

import (
	"testing"
)

// capability_test.go — registry mutation guards: exported accessors must never
// expose live state that a caller could mutate.

// TestCapabilitiesReturnsCopy pins that mutating the slice returned by
// Capabilities() cannot change the live registry.
func TestCapabilitiesReturnsCopy(t *testing.T) {
	original := Capabilities()
	if len(original) == 0 {
		t.Fatal("no capabilities registered")
	}
	original[0].ID = "mutated"
	for _, c := range Capabilities() {
		if c.ID == "mutated" {
			t.Fatal("Capabilities() exposed the live registry")
		}
	}
}

// TestCapabilitiesPanicsWhenEmpty pins the fail-fast guard: a production
// reader must never silently see an empty registry (which would disable the
// hidden-capability disclosure), so Capabilities() panics like
// requireCapabilities.
func TestCapabilitiesPanicsWhenEmpty(t *testing.T) {
	saved := capabilityRegistry
	capabilityRegistry = nil
	defer func() { capabilityRegistry = saved }()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Capabilities() must panic on an empty registry")
		}
	}()
	Capabilities()
}

// TestSchemaColumnsFreshCopies pins that CoreColumns() and StatusColumns() return
// fresh copies: mutating one return value never leaks into the next.
func TestSchemaColumnsFreshCopies(t *testing.T) {
	status := StatusColumns()
	if len(status) == 0 {
		t.Fatal("StatusColumns() returned an empty schema")
	}
	status[0].Header = "MUTATED"
	cols := CoreColumns()
	if len(cols) == 0 {
		t.Fatal("CoreColumns() returned an empty schema")
	}
	cols[0].Header = "MUTATED"
	if got := ColumnHeaders(StatusColumns()); got[0] != "PATH" {
		t.Fatalf("StatusColumns() first header = %q, want PATH", got[0])
	}
	if got := ColumnHeaders(CoreColumns()); got[0] != "PATH" {
		t.Fatalf("CoreColumns() first header = %q, want PATH", got[0])
	}
}

// TestCellIntegrations pins the grouped cell's token composition. It lives in
// core (beside cellIntegrations), so it keeps pinning the cell contract even
// though the function is unexported.
func TestCellIntegrations(t *testing.T) {
	chunk := Capability{ID: "chunkhound", Token: "chunk"}
	mcp := Capability{ID: "mcp", Token: "mcp"}
	ctx := ColumnContext{Active: []Capability{chunk, mcp}}
	cases := []struct {
		name string
		caps map[string]CapState
		want string
	}{
		{"all present", map[string]CapState{"chunkhound": CapPresent, "mcp": CapPresent}, "chunk mcp"},
		{"partial and absent", map[string]CapState{"chunkhound": CapPartial}, "chunk~"},
		{"present and absent", map[string]CapState{"chunkhound": CapPresent}, "chunk"},
		{"empty", nil, "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cellIntegrations(Worktree{Caps: tc.caps}, ctx); got != tc.want {
				t.Errorf("cellIntegrations = %q, want %q", got, tc.want)
			}
		})
	}
	if got := cellIntegrations(Worktree{}, ColumnContext{}); got != "-" {
		t.Errorf("no applicable Capability must render %q, got %q", "-", got)
	}
}
