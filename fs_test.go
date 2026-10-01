package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fs_test.go — Tier-1 contracts for fs.go exercised against the real
// filesystem. No mocks or stubs.

// fsAssertMode skips on Windows where POSIX permission bits are not enforced.
func fsAssertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func TestCanonical(t *testing.T) {
	root := canonical(t.TempDir())

	t.Run("clean", func(t *testing.T) {
		sep := string(filepath.Separator)
		got := canonical(root + sep + "a" + sep + ".." + sep + "x")
		if want := filepath.Join(root, "x"); got != want {
			t.Fatalf("canonical = %q, want %q", got, want)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		once := canonical(filepath.Join(root, "missing"))
		if twice := canonical(once); twice != once {
			t.Fatalf("canonical not idempotent: %q -> %q", once, twice)
		}
	})

	t.Run("symlinked ancestor preserves non-existent tail", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.MkdirAll(filepath.Join(real, "kid"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		mustSymlink(t, real, link)
		got := canonical(filepath.Join(link, "gone"))
		if want := filepath.Join(canonical(real), "gone"); got != want {
			t.Fatalf("canonical = %q, want %q", got, want)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		t.Chdir(root)
		got := canonical("sub/./gone")
		if want := filepath.Join("sub", "gone"); got != want {
			t.Fatalf("canonical = %q, want %q", got, want)
		}
	})
}

// chmodTest pins permission bits regardless of umask, failing on error.
func chmodTest(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestCopyFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	writeFile(t, src, "hello", 0o600)
	chmodTest(t, src, 0o600)

	dst := filepath.Join(t.TempDir(), "dst.txt")
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello" {
		t.Fatalf("content = %q, want hello", b)
	}
	fsAssertMode(t, dst, 0o600)

	writeFile(t, dst, "old", 0o644)
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile overwrite: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello" {
		t.Fatalf("overwritten content = %q, want hello", b)
	}
}

func TestCopyLink(t *testing.T) {
	src := filepath.Join(t.TempDir(), "link")
	mustSymlink(t, "target-file", src)

	dst := filepath.Join(t.TempDir(), "out", "link")
	if err := copyLink(src, dst); err != nil {
		t.Fatalf("copyLink: %v", err)
	}
	if got, err := os.Readlink(dst); err != nil || got != "target-file" {
		t.Fatalf("readlink = (%q,%v), want target-file", got, err)
	}

	// Dangling targets must survive without dereferencing.
	dangle := filepath.Join(t.TempDir(), "dangle")
	mustSymlink(t, "does-not-exist", dangle)
	dst2 := filepath.Join(t.TempDir(), "d2")
	if err := copyLink(dangle, dst2); err != nil {
		t.Fatalf("copyLink dangling: %v", err)
	}
	if got, _ := os.Readlink(dst2); got != "does-not-exist" {
		t.Fatalf("dangling readlink = %q", got)
	}
}

// fsWriteTree seeds a tree with live daemon sidecars (top-level and nested)
// plus sibling files that must survive every copy.
func fsWriteTree(t *testing.T, root string) {
	t.Helper()
	for _, rel := range []string{
		"a.txt", "keep.txt", "daemon.log",
		"sub/b.txt", "sub/daemon.log",
		"watchman/state", "sub/watchman/state",
		".watchman/state", "sub/.watchman/state",
	} {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), rel, 0o644)
	}
}

func fsExcludedRels() []string {
	return []string{"daemon.log", "sub/daemon.log", "watchman", "sub/watchman", ".watchman", "sub/.watchman"}
}

func TestCopyTree(t *testing.T) {
	t.Run("dirs files and modes", func(t *testing.T) {
		src := t.TempDir()
		dst := filepath.Join(t.TempDir(), "dst")
		fsWriteTree(t, src)
		chmodTest(t, filepath.Join(src, "sub"), 0o700)
		chmodTest(t, filepath.Join(src, "a.txt"), 0o640)
		if err := copyTree(src, dst); err != nil {
			t.Fatalf("copyTree: %v", err)
		}
		for _, rel := range []string{"a.txt", "keep.txt", "sub/b.txt"} {
			b, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
			if string(b) != rel {
				t.Fatalf("%s content = %q, want %q", rel, b, rel)
			}
		}
		fsAssertMode(t, filepath.Join(dst, "sub"), 0o700)
		fsAssertMode(t, filepath.Join(dst, "a.txt"), 0o640)
	})

	t.Run("excludes daemon sidecars including nested", func(t *testing.T) {
		src := t.TempDir()
		dst := filepath.Join(t.TempDir(), "dst")
		fsWriteTree(t, src)
		if err := copyTree(src, dst); err != nil {
			t.Fatalf("copyTree: %v", err)
		}
		for _, rel := range fsExcludedRels() {
			if exists(filepath.Join(dst, filepath.FromSlash(rel))) {
				t.Fatalf("excluded %s was copied", rel)
			}
		}
		if !exists(filepath.Join(dst, "keep.txt")) || !exists(filepath.Join(dst, "sub", "b.txt")) {
			t.Fatal("siblings of excluded entries must survive")
		}
	})

	t.Run("does not dereference symlinks", func(t *testing.T) {
		src := t.TempDir()
		dst := filepath.Join(t.TempDir(), "dst")
		writeFile(t, filepath.Join(src, "a.txt"), "A", 0o644)
		mustSymlink(t, "a.txt", filepath.Join(src, "link"))
		if err := copyTree(src, dst); err != nil {
			t.Fatalf("copyTree: %v", err)
		}
		info, err := os.Lstat(filepath.Join(dst, "link"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			t.Fatal("link was dereferenced")
		}
		if got, _ := os.Readlink(filepath.Join(dst, "link")); got != "a.txt" {
			t.Fatalf("readlink = %q, want a.txt", got)
		}
	})
}

// fsWalkFiles snapshots a tree as rel-path -> content, "dir" or "link:<target>".
func fsWalkFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			m[rel] = "link:" + target
		case d.IsDir():
			m[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			m[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// fsDropExcluded removes the entries copyTree/cloneDir must never carry over.
func fsDropExcluded(m map[string]string) {
	for k := range m {
		for _, seg := range strings.Split(k, "/") {
			if seg == "daemon.log" || seg == "watchman" || seg == ".watchman" {
				delete(m, k)
				break
			}
		}
	}
}

func TestCloneDir(t *testing.T) {
	src := t.TempDir()
	fsWriteTree(t, src)
	dst := filepath.Join(t.TempDir(), "clone")
	if err := cloneDir(src, dst); err != nil {
		t.Fatalf("cloneDir: %v", err)
	}
	want := fsWalkFiles(t, src)
	fsDropExcluded(want)
	if got := fsWalkFiles(t, dst); !reflect.DeepEqual(got, want) {
		t.Fatalf("clone tree = %#v, want %#v", got, want)
	}
	for _, rel := range fsExcludedRels() {
		if exists(filepath.Join(dst, filepath.FromSlash(rel))) {
			t.Fatalf("clone kept excluded %s", rel)
		}
	}
}

func TestRemoveExcluded(t *testing.T) {
	root := t.TempDir()
	fsWriteTree(t, root)
	if err := removeExcluded(root); err != nil {
		t.Fatalf("removeExcluded: %v", err)
	}
	for _, rel := range fsExcludedRels() {
		if exists(filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("excluded %s survived", rel)
		}
	}
	for _, rel := range []string{"a.txt", "keep.txt", "sub/b.txt"} {
		if !exists(filepath.Join(root, filepath.FromSlash(rel))) {
			t.Fatalf("kept %s was removed", rel)
		}
	}
}

func TestWriteJSONContract(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.json")
	in := map[string]any{"known": 1, "extra": "x"}
	if err := writeJSON(p, in); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf("must end with newline: %q", b)
	}
	if !bytes.Contains(b, []byte("\n  \"known\"")) || bytes.Contains(b, []byte("\t")) {
		t.Fatalf("must use 2-space indent, got %q", b)
	}
	fsAssertMode(t, p, 0o644)

	var out map[string]json.RawMessage
	if err := readJSON(p, &out); err != nil {
		t.Fatalf("readJSON round-trip: %v", err)
	}
	if string(out["known"]) != "1" || string(out["extra"]) != `"x"` {
		t.Fatalf("round-trip lost keys: %v", out)
	}
}

func TestPrintJSON(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printJSON(map[string]any{"a": 1}); err != nil {
			t.Errorf("printJSON: %v", err)
		}
	})
	if want := "{\n  \"a\": 1\n}\n"; out != want {
		t.Fatalf("printJSON = %q, want %q", out, want)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	writeFile(t, file, "x", 0o644)
	if !exists(file) || !exists(dir) {
		t.Fatal("existing file/dir must report true")
	}
	if exists(filepath.Join(dir, "missing")) {
		t.Fatal("missing path must report false")
	}
}
