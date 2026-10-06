package main

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"
)

// capability_test.go — the integration registry's state model, independent of
// rendering, so a cell refactor cannot silently change what "partial", absent
// and n/a mean.

func TestChunkhoundCapabilityState(t *testing.T) {
	cases := []struct {
		name string
		cfg  bool
		db   bool
		want capState
	}{
		{"none", false, false, capAbsent},
		{"config only", true, false, capPartial},
		{"db only", false, true, capPartial},
		{"both", true, true, capPresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.cfg {
				writeFile(t, filepath.Join(root, chunkhoundConfigFile), "{}", 0o644)
			}
			if tc.db {
				writeFile(t, chunkhoundDBPath(root), "{}", 0o644)
			}
			in := capInput{root: root, mcp: map[string]bool{}}
			if got := chunkhoundCapability.probe(in); got != tc.want {
				t.Fatalf("chunkhound = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCapabilityApplicability pins the n/a distinction: a capability no
// worktree uses is not applicable, while an applicable one absent here is "-".
func TestCapabilityApplicability(t *testing.T) {
	wts := []worktree{
		{Path: "/a", Caps: map[string]capState{"mcp": capPresent}},
		{Path: "/b", Caps: map[string]capState{"chunkhound": capPartial}},
	}
	active := activeCapabilities(wts)
	for _, id := range []string{"mcp", "chunkhound"} {
		if !applicable(active, id) {
			t.Errorf("%s must be applicable: %v", id, active)
		}
	}
	if applicable(active, "claude") {
		t.Errorf("claude must be n/a: %v", active)
	}
	got := integrationStates(wts[0], active)
	// Derive the expected set so registering a new agent does not break a test
	// whose purpose is the n/a vs - distinction.
	want := map[string]string{}
	for _, c := range capabilities() {
		switch c.id {
		case "mcp":
			want[c.id] = "yes"
		case "chunkhound":
			want[c.id] = "-"
		default:
			want[c.id] = "n/a"
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
}

// TestMCPCapabilityEquallySupported pins the equal-support contract: every
// recognized MCP config — shared or harness-native — lights mcp, while the
// config a harness reads also lights that harness. A corrupt config counts as
// absent for both.
func TestMCPCapabilityEquallySupported(t *testing.T) {
	for _, rel := range mcpConfigFiles() {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, rel), "{}", 0o644)
			in := capInput{root: root, mcp: mcpConfigs(root, nil)}
			if got := mcpCapability.probe(in); got != capPresent {
				t.Errorf("%s: mcp = %v, want present", rel, got)
			}
		})
	}
	t.Run("corrupt counts as absent", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, piNativeMCPFile), "{not json", 0o644)
		in := capInput{root: root, mcp: mcpConfigs(root, nil)}
		if got := mcpCapability.probe(in); got != capAbsent {
			t.Errorf("corrupt config: mcp = %v, want absent", got)
		}
	})
}

func TestGroupedCell(t *testing.T) {
	active := []capability{chunkhoundCapability, mcpCapability}
	ctx := columnContext{active: active}
	cases := []struct {
		name string
		caps map[string]capState
		want string
	}{
		{"all present", map[string]capState{"chunkhound": capPresent, "mcp": capPresent}, "chunk mcp"},
		{"partial and absent", map[string]capState{"chunkhound": capPartial}, "chunk~"},
		{"present and absent", map[string]capState{"chunkhound": capPresent}, "chunk"},
		{"empty", nil, "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cellIntegrations(worktree{Caps: tc.caps}, ctx); got != tc.want {
				t.Errorf("cellIntegrations = %q, want %q", got, tc.want)
			}
		})
	}
	if got := cellIntegrations(worktree{}, columnContext{}); got != "-" {
		t.Errorf("no applicable capability must render %q, got %q", "-", got)
	}
}

// TestCapabilityTokens pins the token contract: a token is a unique, 2+ char,
// case-insensitive prefix of its id. "mc" for "mcp" violates it — it is not a
// prefix — which is exactly the unreadability the prefix rule forbids.
func TestCapabilityTokens(t *testing.T) {
	seen, seenID := map[string]bool{}, map[string]bool{}
	for _, c := range capabilities() {
		tok := strings.ToLower(c.token)
		switch {
		case len(c.token) < 2:
			t.Errorf("%s token %q: want at least 2 chars", c.id, c.token)
		case !strings.HasPrefix(strings.ToLower(c.id), tok):
			t.Errorf("%s token %q: want a prefix of the id", c.id, c.token)
		case seen[tok]:
			t.Errorf("%s token %q: duplicate", c.id, c.token)
		}
		if seenID[c.id] {
			t.Errorf("%s: duplicate capability id", c.id)
		}
		seen[tok], seenID[c.id] = true, true
	}
}

// TestCapabilityHeadersDoNotCollide pins the registry's collision guard: the
// real capability set must build, and a header that would overwrite a core
// column must panic instead of silently changing its shrink behavior.
func TestCapabilityHeadersDoNotCollide(t *testing.T) {
	_ = columnRegistryFor(capabilities())
	defer func() {
		if recover() == nil {
			t.Fatal("colliding capability header must panic")
		}
	}()
	columnRegistryFor([]capability{{id: "path", token: "path"}})
}

// TestAnyMCPConfig pins the presence helper as the SSOT for the status signal.
func TestAnyMCPConfig(t *testing.T) {
	if anyMCPConfig(mcpConfigs(t.TempDir(), nil)) {
		t.Fatal("empty root must not be MCP-configured")
	}
	for _, rel := range mcpConfigFiles() {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, rel), "{}", 0o644)
		if !anyMCPConfig(mcpConfigs(root, nil)) {
			t.Fatalf("%s must mark the root configured", rel)
		}
	}
}
