package harness

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { RegisterCapabilities(); os.Exit(m.Run()) }
