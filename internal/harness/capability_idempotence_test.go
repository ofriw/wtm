package harness

import (
	"testing"

	"wtm/internal/core"
)

// TestRegisterCapabilitiesIdempotent pins the documented idempotence contract:
// cli.Run may call RegisterCapabilities repeatedly, and the registry must not
// grow duplicates. ID order is registry order: project integrations first
// (chunkhound, mcp), then agents in agents() order.
func TestRegisterCapabilitiesIdempotent(t *testing.T) {
	RegisterCapabilities()
	RegisterCapabilities()
	caps := core.Capabilities()
	want := []string{"chunkhound", "mcp"}
	for _, a := range agents() {
		want = append(want, a.name)
	}
	if len(caps) != len(want) {
		t.Fatalf("len(core.Capabilities()) = %d, want %d", len(caps), len(want))
	}
	for i, c := range caps {
		if c.ID != want[i] {
			t.Fatalf("capability[%d].ID = %q, want %q", i, c.ID, want[i])
		}
	}
}
