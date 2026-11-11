//go:build !windows

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestFallbackAttemptDirIsPrivate requires a non-symlink directory with mode
// 0700, matching the Unix permission check without asserting ownership.
func TestFallbackAttemptDirIsPrivate(t *testing.T) {
	dir, err := ensureFallbackDir()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("fallback path is not a directory: %v", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("fallback dir mode = %o, want 700", got)
	}
}

// TestFetchCoolingDownRejectsFIFOMarker pins the no-open rule: a FIFO at the
// marker path must not block the status command.
func TestFetchCoolingDownRejectsFIFOMarker(t *testing.T) {
	path := isolatedRatesCache(t)
	dir, err := ensureFallbackDir()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, fallbackAttemptName(path))
	if err := unix.Mkfifo(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(marker) })

	done := make(chan struct{})
	var cooling bool
	go func() {
		cooling, _ = fetchCoolingDown(path)
		close(done)
	}()
	select {
	case <-done:
		if cooling {
			t.Error("a FIFO marker must not cool down")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fetchCoolingDown blocked on a FIFO marker")
	}
}

// TestMarkerWriteFailureWarns pins the disclosure: when neither marker location
// can be written, the run still reports the fetch failure and names the failed
// cooldown, so the operator can see why later runs stall.
func TestMarkerWriteFailureWarns(t *testing.T) {
	_, _ = blockRatesCacheWrite(t, true)
	// Point TMPDIR at a regular file, so the fallback directory cannot exist.
	tmp := filepath.Join(t.TempDir(), "tmpdir-file")
	writeFile(t, tmp, "x", 0o644)
	t.Setenv("TMPDIR", tmp)
	url := serveRates(t, http.StatusInternalServerError, "")
	var loadErr error
	errOut := captureStderr(t, func() { _, loadErr = loadCatalogFrom(url) })
	if loadErr == nil {
		t.Fatal("a failed fetch with no cache must report an error")
	}
	if !strings.Contains(errOut, "rate cooldown marker write failed") {
		t.Fatalf("stderr must disclose the marker failure:\n%s", errOut)
	}
}
