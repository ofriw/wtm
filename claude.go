package main

// claude.go — the Claude Code agent descriptor.

const (
	claudeSettingsFile      = ".claude/settings.json"
	claudeLocalSettingsFile = ".claude/settings.local.json"
	claudeWorktreesDir      = ".claude/worktrees"
)

// claudeAgent shares the repo-root project MCP file with Pi's pi-mcp-adapter.
// `.claude/worktrees` holds Claude's own nested worktrees, so seeding must
// never copy it into a new checkout.
var claudeAgent = agent{
	name:          "claude",
	statusToken:   "claude",
	statusFiles:   []string{claudeSettingsFile, claudeLocalSettingsFile},
	mcpFiles:      []string{projectMCPFile},
	seedFiles:     []string{"CLAUDE.local.md"},
	seedDir:       ".claude",
	seedSkip:      []string{claudeWorktreesDir},
	configDirEnv:  "CLAUDE_CONFIG_DIR",
	configDirRel:  []string{".claude"},
	sessionSubdir: "projects",
	sessionOwner:  claudeSessionOwner,
}
