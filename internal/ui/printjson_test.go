package ui

import (
	"testing"

	. "wtm/internal/testutil"
)

// TestPrintJSON pins the machine-output contract: indented JSON on stdout.
func TestPrintJSON(t *testing.T) {
	out := CaptureStdout(t, func() {
		if err := PrintJSON(map[string]any{"a": 1}); err != nil {
			t.Errorf("PrintJSON: %v", err)
		}
	})
	if want := "{\n  \"a\": 1\n}\n"; out != want {
		t.Fatalf("PrintJSON = %q, want %q", out, want)
	}
}
