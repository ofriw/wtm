package core

import (
	"os"
	"testing"
)

// TestMain registers the capability descriptors the core UI tests assume
// (harness registers them in production; the core test binary must do it
// itself because it cannot import harness without an import cycle).
//
// These are synthetic probes that unconditionally report CapAbsent: they only
// satisfy the registry's non-empty guard so core's table/JSON code paths can
// be exercised. They do not model real harness probes — those live in harness's
// own tests against real config layouts.
func TestMain(m *testing.M) {
	RegisterCapability(Capability{ID: "chunkhound", Token: "chunk", Probe: func(CapInput) CapState { return CapAbsent }})
	RegisterCapability(Capability{ID: "mcp", Token: "mcp", Probe: func(CapInput) CapState { return CapAbsent }})
	os.Exit(m.Run())
}
