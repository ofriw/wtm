//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// control_file_unix_test.go — the hostile-file cases: a FIFO or a symlink to a
// FIFO must never hang a control-file reader. The type check runs on the opened
// handle (fs.go openRegularFile), so refusal happens without blocking and
// without a window where the path can be swapped.

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRefusedQuickly(t *testing.T, read func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- read() }()
	select {
	case err := <-done:
		if !errors.Is(err, errNotRegularFile) {
			t.Fatalf("read = %v, want errNotRegularFile", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guarded read blocked on a FIFO")
	}
}

func TestReadControlFileRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	mkfifo(t, path)
	assertRefusedQuickly(t, func() error {
		_, err := readControlFile(path)
		return err
	})
}

func TestReadControlFileRejectsSymlinkToFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	mkfifo(t, fifo)
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	assertRefusedQuickly(t, func() error {
		_, err := readControlFile(link)
		return err
	})
}

// TestLoadCatalogRejectsFIFO pins the status hang: a FIFO at WTM_RATES is the
// catalog path status cannot work without, so its read must refuse the handle
// instead of blocking forever in open(2).
func TestLoadCatalogRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	mkfifo(t, path)
	t.Setenv(ratesEnv, path)
	assertRefusedQuickly(t, func() error {
		_, err := loadCatalog()
		return err
	})
}

// TestLoadCatalogRejectsSymlinkToFIFO covers the same hang behind a link, which
// is the shape a copied or committed cache path takes.
func TestLoadCatalogRejectsSymlinkToFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "rates.fifo")
	mkfifo(t, fifo)
	link := filepath.Join(dir, "rates.json")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ratesEnv, link)
	assertRefusedQuickly(t, func() error {
		_, err := loadCatalog()
		return err
	})
}

func TestReadControlFileAllowsSymlinkToRegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	writeFile(t, target, `{"ok":true}`, 0o600)
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	b, err := readControlFile(link)
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("symlinked control file = (%q, %v), want the target body", b, err)
	}
}

func TestReadControlFileRejectsSymlinkToDevice(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatal(err)
	}
	// A device opens cleanly, so only a check on the handle can refuse it.
	assertRefusedQuickly(t, func() error {
		_, err := readControlFile(link)
		return err
	})
}

func TestReadControlFileRefusesFIFODespiteWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	mkfifo(t, path)
	// O_RDWR keeps a live writer on the FIFO without a blocking handshake, so a
	// type check that came after the open would read forever instead of refusing.
	writer, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	assertRefusedQuickly(t, func() error {
		_, err := readControlFile(path)
		return err
	})
}

func TestReadClaudeAccountRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	mkfifo(t, path)
	assertRefusedQuickly(t, func() error {
		_, err := readClaudeAccount(path)
		return err
	})
}

// TestValidMCPConfigRejectsFIFO pins the unconditional repo vector: a committed
// .mcp.json symlink/FIFO must be refused with a warning, never opened.
func TestValidMCPConfigRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), projectMCPFile)
	mkfifo(t, path)
	events := make(chan progressEvent, 1)
	p := &progress{ctx: context.Background(), events: events}
	done := make(chan bool, 1)
	go func() { done <- validMCPConfig(path, p) }()
	select {
	case valid := <-done:
		if valid {
			t.Fatal("a FIFO MCP config must count as absent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("validMCPConfig blocked on a FIFO")
	}
	select {
	case ev := <-events:
		if ev.kind != evNotice || !strings.Contains(ev.text, projectMCPFile) || !strings.Contains(ev.text, "not a regular file") {
			t.Fatalf("warning = %+v, want the path and the refusal reason", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO MCP config was refused without a warning")
	}
}

// TestReadControlFileCapsSymlinkTarget pins the review case: a symlink to a
// large regular file must hit the size cap, not read past it.
func TestReadControlFileCapsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	writeFile(t, target, strings.Repeat("a", maxControlFileBytes+1), 0o600)
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := readControlFile(link)
	if !errors.Is(err, errFileTooLarge) {
		t.Fatalf("oversized symlink target = %v, want errFileTooLarge", err)
	}
}

// wtmHome points HOME at a temp dir and returns its .wtm store directory.
func wtmHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".wtm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// controlStores names the two command-wide control files and their readers.
func controlStores() []struct {
	name string
	file string
	read func() error
} {
	return []struct {
		name string
		file string
		read func() error
	}{
		{"settings", "settings.json", func() error { _, _, err := loadSettings(); return err }},
		{"temp store", "temp.json", func() error { _, err := readTempStoreUnlocked(); return err }},
	}
}

// TestControlStoresRejectFIFO pins the command-wide hang: status, add, gc and
// delete all read these stores, so a FIFO must be refused before open(2) blocks.
func TestControlStoresRejectFIFO(t *testing.T) {
	for _, tc := range controlStores() {
		t.Run(tc.name, func(t *testing.T) {
			mkfifo(t, filepath.Join(wtmHome(t), tc.file))
			assertRefusedQuickly(t, tc.read)
		})
	}
}

// TestControlStoresRejectOversizedFile pins the size cap on the command-wide
// readers, not just the primitive.
func TestControlStoresRejectOversizedFile(t *testing.T) {
	for _, tc := range controlStores() {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(wtmHome(t), tc.file)
			writeFile(t, path, strings.Repeat("a", maxControlFileBytes+1), 0o600)
			if err := tc.read(); !errors.Is(err, errFileTooLarge) {
				t.Fatalf("%s = %v, want errFileTooLarge", tc.file, err)
			}
		})
	}
}
