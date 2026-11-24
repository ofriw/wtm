package harness

import (
	"testing"

	. "wtm/internal/testutil"
)

// equivalence_test.go — pins that testutil's mirrored helpers (kept leaf so
// testutil never imports a production package) stay equal to the production
// logic they mirror.

// TestAgentConfigDirMatchesHarness pins testutil.AgentConfigDir against each
// harness agent's ConfigDir. The env override branch is skipped: both sides
// read the same env var, so only the home+rel fallback can drift.
func TestAgentConfigDirMatchesHarness(t *testing.T) {
	Sandbox(t)
	for _, a := range agents() {
		t.Setenv(a.configDirEnv, "")
		want := a.ConfigDir()
		if got := AgentConfigDir(a.configDirEnv, a.configDirRel...); got != want {
			t.Fatalf("%s: AgentConfigDir = %q, want %q", a.name, got, want)
		}
	}
}

// TestAgentSessionSubdirsMatchConstants pins the exported subdir constants to
// the agent descriptors, so the cli discovery fixtures (which use the
// constants) cannot drift from the real session roots.
func TestAgentSessionSubdirsMatchConstants(t *testing.T) {
	if got := ClaudeAgent.sessionSubdir; got != ClaudeSessionSubdir {
		t.Fatalf("ClaudeAgent.sessionSubdir = %q, want %q", got, ClaudeSessionSubdir)
	}
	if got := PiAgent.sessionSubdir; got != PiSessionSubdir {
		t.Fatalf("PiAgent.sessionSubdir = %q, want %q", got, PiSessionSubdir)
	}
}
