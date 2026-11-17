package harness

import (
	"os"
	"testing"

	"wtm/internal/testutil"
)

func TestMain(m *testing.M) {
	RegisterCapabilities()
	code := m.Run()
	testutil.CleanupWTM()
	os.Exit(code)
}
