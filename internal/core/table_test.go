package core

import (
	"testing"
	"time"
	. "wtm/internal/testutil"

	"wtm/internal/ui"
)

// table_test.go — grid-engine integration contracts exercised through the real
// column registry; the engine's own pure contracts live in internal/ui.

func TestColumnWidths(t *testing.T) {
	headers := ColumnHeaders(StatusColumns())
	rows := [][]string{{
		"/a/very/long/checkout/path/that/keeps/going/on",
		"feature/a-rather-long-branch",
		"origin/feature/a-rather-long-branch",
		"2020-01-01", "UNUSED", "ch mc",
	}}
	natural := ui.NaturalWidths(headers, rows)
	naturalTotal := ui.LineWidth(natural)

	t.Run("budget zero is natural", func(t *testing.T) {
		assertWidths(t, widthsFor(headers, rows, 0), natural)
	})
	t.Run("generous budget is natural", func(t *testing.T) {
		assertWidths(t, widthsFor(headers, rows, naturalTotal+40), natural)
	})
	t.Run("path shrinks before branch", func(t *testing.T) {
		budget := naturalTotal - 4
		got := widthsFor(headers, rows, budget)
		assertWithin(t, got, budget)
		assertPinned(t, got, natural)
		if got[0] >= natural[0] {
			t.Errorf("PATH should shrink first: got %d, natural %d", got[0], natural[0])
		}
		if got[1] != natural[1] || got[2] != natural[2] {
			t.Errorf("BRANCH/UPSTREAM must not shrink while PATH has room: got %d/%d, natural %d/%d", got[1], got[2], natural[1], natural[2])
		}
	})
	t.Run("upstream shrinks after path floor", func(t *testing.T) {
		pathFloor := ColumnRegistry()["PATH"].Floor
		budget := naturalTotal - (natural[0] - pathFloor) - 3
		got := widthsFor(headers, rows, budget)
		assertWithin(t, got, budget)
		assertPinned(t, got, natural)
		if got[0] != pathFloor {
			t.Errorf("PATH = %d, want floor %d", got[0], pathFloor)
		}
		if got[2] != natural[2]-3 {
			t.Errorf("UPSTREAM = %d, want %d", got[2], natural[2]-3)
		}
	})
	t.Run("tight budget still fits and keeps pinned columns", func(t *testing.T) {
		got := widthsFor(headers, rows, 45)
		assertWithin(t, got, 45)
		assertPinned(t, got, natural)
	})
}

func assertWidths(t *testing.T, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("width count = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("width[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func assertPinned(t *testing.T, got, natural []int) {
	t.Helper()
	for _, i := range []int{4} {
		if got[i] != natural[i] {
			t.Errorf("pinned column %d = %d, want %d", i, got[i], natural[i])
		}
	}
}

func assertWithin(t *testing.T, widths []int, budget int) {
	t.Helper()
	if got := ui.LineWidth(widths); got > budget {
		t.Errorf("lineWidth = %d, exceeds budget %d", got, budget)
	}
}

func widthsFor(headers []string, rows [][]string, budget int) []int {
	p := GridPolicy(headers)
	return ui.ColumnWidths(headers, rows, budget, p.Priority, p.Floors, p.CapHeader)
}

// TestValidateProgress pins the --progress value contract: valid modes are
// accepted, and an unknown mode is a usage error.
func TestValidateProgress(t *testing.T) {
	for _, mode := range []string{"", ui.ProgressAuto, ui.ProgressAlways, ui.ProgressNever} {
		if err := validateProgress(mode); err != nil {
			t.Errorf("validateProgress(%q) = %v, want nil", mode, err)
		}
	}
	var ue UsageError
	if err := validateProgress("sometimes"); !errors.As(err, &ue) {
		t.Errorf("validateProgress(sometimes) = %v, want usageError", err)
	}
}

// TestCellTemp pins the shared TEMP cell: a dash for permanent worktrees, the
// compact remaining window while active, "expired" once the window has passed.
func TestCellTemp(t *testing.T) {
	now := TempTestTime
	cases := []struct {
		name string
		w    Worktree
		want string
	}{
		{"permanent is dash", Worktree{Branch: "main"}, "-"},
		{"active temp shows remaining window", Worktree{Temp: true, TempCreated: now.Add(-2 * time.Hour), TempTTL: 3 * time.Hour}, "1h"},
		{"lapsed temp reads expired", Worktree{Temp: true, TempCreated: now.Add(-3 * time.Hour), TempTTL: time.Hour}, "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cellTemp(tc.w, ColumnContext{Now: now}); got != tc.want {
				t.Fatalf("cellTemp = %q, want %q", got, tc.want)
			}
		})
	}
}
