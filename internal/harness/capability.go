package harness

import (
	"path/filepath"

	"wtm/internal/core"
	"wtm/internal/fs"
)

// capability.go — the integration descriptors and their registration. The
// model and evaluation live in core; this package owns the probes, so a new
// integration is one descriptor, never a renderer or schema edit. Registration
// is explicit at the composition root (cli.Run), never an init.

var capabilitiesRegistered bool

// RegisterCapabilities registers every integration descriptor in registry order
// (project integrations first, then agents). Idempotent: cli.Run may call it
// repeatedly in one process. The flag flips only after core's one-shot
// registration succeeds, so a panic mid-registration leaves it false.
func RegisterCapabilities() {
	if capabilitiesRegistered {
		return
	}
	caps := []core.Capability{chunkhoundCapability, mcpCapability}
	for _, a := range agents() {
		caps = append(caps, a.capability())
	}
	core.RegisterCapabilities(caps)
	capabilitiesRegistered = true
}

var (
	// chunkhound is partial when only one of config/db exists: a half-set-up
	// workspace is a state the old CONFIG/DB columns could not express.
	chunkhoundCapability = core.Capability{
		ID: "chunkhound", Token: "chunk",
		Probe: func(in core.CapInput) core.CapState {
			cfg := fs.Exists(filepath.Join(in.Root, ChunkhoundConfigFile))
			db := fs.Exists(ChunkhoundDBPath(in.Root))
			return core.Presence(cfg && db, cfg || db)
		},
	}
	// mcp is MCP support of any kind: every recognized MCP config counts
	// equally (the shared project file, a harness's native config), so a
	// worktree wired through any one of them reads mcp=yes. The config that
	// wires a harness additionally lights that harness's own capability.
	mcpCapability = core.Capability{
		ID: "mcp", Token: "mcp",
		Probe: func(in core.CapInput) core.CapState { return core.PresentIf(anyMCPConfig(in.MCP)) },
	}
)
