package harness

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"wtm/internal/core"
	. "wtm/internal/testutil"
)

// capability_test.go — the integration registry's state model, independent of
// rendering, so a cell refactor cannot silently change what "partial", absent
// and n/a mean.

func TestChunkhoundCapabilityState(t *testing.T) {
	cases := []struct {
		name string
		cfg  bool
		db   bool
		want core.CapState
	}{
		{"none", false, false, core.CapAbsent},
		{"config only", true, false, core.CapPartial},
		{"db only", false, true, core.CapPartial},
		{"both", true, true, core.CapPresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.cfg {
				WriteFile(t, filepath.Join(root, ChunkhoundConfigFile), "{}", 0o644)
			}
			if tc.db {
				WriteFile(t, ChunkhoundDBPath(root), "{}", 0o644)
			}
			in := core.CapInput{Root: root, MCP: map[string]bool{}}
			if got := chunkhoundCapability.Probe(in); got != tc.want {
				t.Fatalf("chunkhound = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCapabilityApplicability pins the n/a distinction: a core.Capability no
// core.Worktree uses is not applicable, while an applicable one absent here is "-".
func TestCapabilityApplicability(t *testing.T) {
	wts := []core.Worktree{
		{Path: "/a", Caps: map[string]core.CapState{"mcp": core.CapPresent}},
		{Path: "/b", Caps: map[string]core.CapState{"chunkhound": core.CapPartial}},
	}
	active := core.ActiveCapabilities(wts)
	for _, id := range []string{"mcp", "chunkhound"} {
		if !core.Applicable(active, id) {
			t.Errorf("%s must be applicable: %v", id, active)
		}
	}
	if core.Applicable(active, "claude") {
		t.Errorf("claude must be n/a: %v", active)
	}
	got := core.IntegrationStates(wts[0], active)
	// Derive the expected set so registering a new agent does not break a test
	// whose purpose is the n/a vs - distinction.
	want := map[string]string{}
	for _, c := range core.Capabilities() {
		switch c.ID {
		case "mcp":
			want[c.ID] = "yes"
		case "chunkhound":
			want[c.ID] = "-"
		default:
			want[c.ID] = "n/a"
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
	for _, rel := range MCPConfigFiles() {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			WriteFile(t, filepath.Join(root, rel), "{}", 0o644)
			in := core.CapInput{Root: root, MCP: MCPConfigs(root, nil)}
			if got := mcpCapability.Probe(in); got != core.CapPresent {
				t.Errorf("%s: mcp = %v, want present", rel, got)
			}
		})
	}
	t.Run("corrupt counts as absent", func(t *testing.T) {
		root := t.TempDir()
		WriteFile(t, filepath.Join(root, PiNativeMCPFile), "{not json", 0o644)
		in := core.CapInput{Root: root, MCP: MCPConfigs(root, nil)}
		if got := mcpCapability.Probe(in); got != core.CapAbsent {
			t.Errorf("corrupt config: mcp = %v, want absent", got)
		}
	})
}

// TestCapabilityTokens pins the token contract: a token is a unique, 2+ char,
// case-insensitive prefix of its id. "mc" for "mcp" violates it — it is not a
// prefix — which is exactly the unreadability the prefix rule forbids.
func TestCapabilityTokens(t *testing.T) {
	seen, seenID := map[string]bool{}, map[string]bool{}
	for _, c := range core.Capabilities() {
		tok := strings.ToLower(c.Token)
		switch {
		case len(c.Token) < 2:
			t.Errorf("%s token %q: want at least 2 chars", c.ID, c.Token)
		case !strings.HasPrefix(strings.ToLower(c.ID), tok):
			t.Errorf("%s token %q: want a prefix of the id", c.ID, c.Token)
		case seen[tok]:
			t.Errorf("%s token %q: duplicate", c.ID, c.Token)
		}
		if seenID[c.ID] {
			t.Errorf("%s: duplicate core.Capability id", c.ID)
		}
		seen[tok], seenID[c.ID] = true, true
	}
}

// TestCapabilityHeadersDoNotCollide pins the registry's collision guard: the
// real core.Capability set must build, and a header that would overwrite a core
// column must panic instead of silently changing its shrink behavior.
func TestCapabilityHeadersDoNotCollide(t *testing.T) {
	_ = core.ColumnRegistryFor(core.Capabilities())
	defer func() {
		if recover() == nil {
			t.Fatal("colliding core.Capability header must panic")
		}
	}()
	core.ColumnRegistryFor([]core.Capability{{ID: "path", Token: "path"}})
}

// TestAnyMCPConfig pins the presence helper as the SSOT for the status signal.
func TestAnyMCPConfig(t *testing.T) {
	if anyMCPConfig(MCPConfigs(t.TempDir(), nil)) {
		t.Fatal("empty root must not be MCP-configured")
	}
	for _, rel := range MCPConfigFiles() {
		root := t.TempDir()
		WriteFile(t, filepath.Join(root, rel), "{}", 0o644)
		if !anyMCPConfig(MCPConfigs(root, nil)) {
			t.Fatalf("%s must mark the root configured", rel)
		}
	}
}
