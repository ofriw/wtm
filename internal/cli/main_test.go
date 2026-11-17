package cli

import (
	"os"
	"testing"

	"wtm/internal/harness"
)

func TestMain(m *testing.M) { harness.RegisterCapabilities(); os.Exit(m.Run()) }
