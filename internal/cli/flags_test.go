package cli

import (
	"errors"
	"testing"

	"wtm/internal/core"
	"wtm/internal/ui"
)

// TestValidateProgress pins the --progress value contract: valid modes are
// accepted, and an unknown mode is a usage error.
func TestValidateProgress(t *testing.T) {
	for _, mode := range []string{"", ui.ProgressAuto, ui.ProgressAlways, ui.ProgressNever} {
		if err := validateProgress(mode); err != nil {
			t.Errorf("validateProgress(%q) = %v, want nil", mode, err)
		}
	}
	var ue core.UsageError
	if err := validateProgress("sometimes"); !errors.As(err, &ue) {
		t.Errorf("validateProgress(sometimes) = %v, want usageError", err)
	}
}
