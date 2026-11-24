package fs

import (
	"testing"

	. "wtm/internal/testutil"
)

// TestSettingsFileMatchesFsLayout pins the testutil fixture path to the fs
// layout SSOT, so a layout rename cannot make fixtures seed a path production
// never reads.
func TestSettingsFileMatchesFsLayout(t *testing.T) {
	got := SettingsFile(t)
	want, err := WtmFilePath(SettingsFileName)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("SettingsFile(t) = %q, want %q", got, want)
	}
}
