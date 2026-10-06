package main

// pi.go — the Pi.dev agent descriptor.

// Pi reads per-project MCP servers from two mutually-exclusive configs:
// pi-mcp-adapter reads the repo-root project MCP file (shared with Claude
// Code); Pi's built-in MCP reads `.pi/mcp.json`, gated by project trust. Either
// may hold secrets, so wtm seeds both even when git ignores them.
const piNativeMCPFile = ".pi/mcp.json"

var piAgent = agent{
	name:          "pi",
	statusToken:   "pi",
	statusFiles:   []string{piNativeMCPFile},
	mcpFiles:      []string{projectMCPFile, piNativeMCPFile},
	seedDir:       ".pi",
	configDirEnv:  "PI_CODING_AGENT_DIR",
	configDirRel:  []string{".pi", "agent"},
	sessionSubdir: "sessions",
	sessionOwner:  piSessionOwner,
}
