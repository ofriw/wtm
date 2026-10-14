//go:build !windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// renameReplace on POSIX is os.Rename: an open reader never blocks replace,
// and the held handle keeps the pre-replace snapshot.
func TestRenameReplaceOverOpenReader(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "store.json")
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil {
		t.Fatalf("write dest: %v", err)
	}
	held, err := os.Open(dest)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() { _ = held.Close() }()

	src := filepath.Join(t.TempDir(), "src.json")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := renameReplace(src, dest); err != nil {
		t.Fatalf("renameReplace over open reader: %v", err)
	}
	got, err := io.ReadAll(held)
	if err != nil {
		t.Fatalf("read held handle: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("held handle = %q, want %q", got, "old")
	}
	fresh, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(fresh) != "new" {
		t.Fatalf("dest = %q, want %q", fresh, "new")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src still exists after replace: %v", err)
	}
}

// Missing source passes the error through and never creates dest.
func TestRenameReplaceMissingSource(t *testing.T) {
	dir := t.TempDir()
	if err := renameReplace(filepath.Join(dir, "nope.json"), filepath.Join(dir, "dest.json")); err == nil {
		t.Fatal("renameReplace with missing source succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "dest.json")); !os.IsNotExist(err) {
		t.Fatalf("dest exists after failed replace: %v", err)
	}
}
