package main

import (
	"encoding/json"
	"testing"
)

// claude_activity_test.go — the user-content classifier is a type test, not a
// text search. A prompt that spells tool_result must still close the prior turn.

func TestClaudeUserKindUsesBlockType(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    activityKind
	}{
		{"human text spells tool_result", `[{"type":"text","text":"tool_result"}]`, activityHuman},
		{"real tool result", `[{"type":"tool_result","content":"ok"}]`, activityWork},
		{"string content", `"tool_result"`, activityHuman},
		{"mixed blocks", `[{"type":"text","text":"x"},{"type":"tool_result"}]`, activityWork},
		{"malformed array", `[{"type":"text"`, activityHuman},
		{"empty array", `[]`, activityHuman},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeUserKind(json.RawMessage(tc.content)); got != tc.want {
				t.Fatalf("claudeUserKind(%s) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}
