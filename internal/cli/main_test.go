package cli

import (
	"os"
	"testing"

	"wtm/internal/harness"
	"wtm/internal/testutil"
)

func TestMain(m *testing.M) {
	harness.RegisterCapabilities()
	code := m.Run()
	testutil.CleanupWTM()
	os.Exit(code)
}
