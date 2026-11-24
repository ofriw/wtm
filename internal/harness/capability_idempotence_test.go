package harness

import (
	"testing"

	"wtm/internal/core"
)

// TestRegisterCapabilitiesIdempotent pins the documented idempotence contract:
// cli.Run may call RegisterCapabilities repeatedly, and the registry must not
// grow duplicates.
func TestRegisterCapabilitiesIdempotent(t *testing.T) {
	RegisterCapabilities()
	before := len(core.AllColumns())
	RegisterCapabilities()
	after := len(core.AllColumns())
	if before != after {
		t.Fatalf("second RegisterCapabilities changed registry size: %d -> %d", before, after)
	}
}
