package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	. "wtm/internal/testutil"
)

// fs_test.go — unit tests for the atomic write and file lock primitives.
// These are the foundation of the temp store's correctness.

func TestWriteFileAtomicSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	data := []byte(`{"key": "value"}`)

	if err := WriteFileAtomic(path, data, 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content = %q, want %q", got, data)
	}

	fsAssertMode(t, path, 0o600)
}

func TestWriteFileAtomicCreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "test.json")

	if err := WriteFileAtomic(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	if !Exists(path) {
		t.Fatal("file not created")
	}
}

func TestWriteFileAtomicCleansUpTempFileOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	// The sibling temp file can be written, but cannot replace a directory.
	blocker := filepath.Join(dir, "test.json")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}

	err := WriteFileAtomic(blocker, []byte("data"), 0o600)
	if err == nil {
		t.Fatal("expected write failure")
	}

	assertAtomicDirectory(t, dir, filepath.Base(blocker), true)
}

func TestWriteFileAtomicOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}

	fsAssertMode(t, path, 0o600)
}

// TestReadFileShared pins the primitive contract every store reader relies on:
// it returns file contents and reports a missing file as os.ErrNotExist.
func TestReadFileShared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFileShared(path)
	if err != nil {
		t.Fatalf("ReadFileShared: %v", err)
	}
	if string(got) != `{"v":1}` {
		t.Fatalf("content = %q, want %q", got, `{"v":1}`)
	}

	if _, err := ReadFileShared(filepath.Join(dir, "missing.json")); !os.IsNotExist(err) {
		t.Fatalf("missing file err = %v, want os.IsNotExist", err)
	}
}

func TestWriteFileAtomicConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.json")
	payloads := atomicTestPayloads()
	if err := WriteFileAtomic(path, []byte(payloads[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	exerciseAtomicReadWrite(t, path, payloads)
	assertAtomicDirectory(t, dir, filepath.Base(path), false)
}

func exerciseAtomicReadWrite(t *testing.T, path string, payloads []string) {
	t.Helper()
	stop, ready := make(chan struct{}), make(chan struct{})
	reads := make(chan error, 1)
	go checkAtomicReads(path, payloads, stop, ready, reads)
	<-ready
	runAtomicWriters(t, path, payloads)
	close(stop)
	if err := <-reads; err != nil {
		t.Fatal(err)
	}
}

func atomicTestPayloads() []string {
	payloads := make([]string, 10)
	for i := range payloads {
		payloads[i] = fmt.Sprintf(`{"writer":%d,"data":%q}`, i, strings.Repeat(string(rune('a'+i)), 64*1024))
	}
	return payloads
}

func checkAtomicPayload(path string, payloads []string) error {
	data, err := ReadFileShared(path)
	if err != nil {
		return err
	}
	for _, payload := range payloads {
		if string(data) == payload {
			return nil
		}
	}
	return fmt.Errorf("atomic read returned an unsubmitted or incomplete payload (%d bytes)", len(data))
}

func checkAtomicReads(path string, payloads []string, stop, ready chan struct{}, result chan<- error) {
	err := checkAtomicPayload(path, payloads)
	close(ready)
	for err == nil {
		select {
		case <-stop:
			result <- checkAtomicPayload(path, payloads)
			return
		default:
			err = checkAtomicPayload(path, payloads)
		}
	}
	result <- err
}

func runAtomicWriters(t *testing.T, path string, payloads []string) {
	t.Helper()
	var wg sync.WaitGroup
	for _, payload := range payloads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if err := WriteFileAtomic(path, []byte(payload), 0o600); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func assertAtomicDirectory(t *testing.T, dir, name string, isDir bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name || entries[0].IsDir() != isDir {
		t.Fatalf("directory must contain only %q (directory=%v): %v", name, isDir, entries)
	}
}

func TestWithFileLockCallbackErrorStillUnlocks(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")
	want := errors.New("callback error")

	err := WithFileLock(lockPath, func() error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}

	// Second call must succeed (lock was released)
	called := false
	if err := WithFileLock(lockPath, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("second lock: %v", err)
	}
	if !called {
		t.Fatal("callback not called")
	}
}

func TestWithFileLockCreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "nested", "deep", "test.lock")

	called := false
	if err := WithFileLock(lockPath, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithFileLock: %v", err)
	}
	if !called {
		t.Fatal("callback not called")
	}

	if !Exists(lockPath) {
		t.Fatal("lock file not created")
	}

	fsAssertMode(t, lockPath, 0o600)
}

func TestWithFileLockSerializesCallbacks(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")
	const n = 5

	// OS locks do not establish Go's happens-before relation; counters must be atomic.
	var active, completed atomic.Int32
	runLockCallbacks(t, lockPath, n, func() error {
		defer active.Add(-1)
		if active.Add(1) != 1 {
			return errors.New("lock callbacks overlapped")
		}
		runtime.Gosched()
		completed.Add(1)
		return nil
	})
	if got := completed.Load(); got != n {
		t.Fatalf("completed callbacks = %d, want %d", got, n)
	}
}

func runLockCallbacks(t *testing.T, path string, count int, callback func() error) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WithFileLock(path, callback); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// TestCanonicalPathMatchesTestutil pins that testutil.CanonicalPath (the
// hermetic-fixture helper, kept leaf) matches the production fs.Canonical.
func TestCanonicalPathMatchesTestutil(t *testing.T) {
	for _, p := range []string{t.TempDir(), "/no/such/path", "."} {
		if got, want := Canonical(p), CanonicalPath(p); got != want {
			t.Fatalf("Canonical(%q) = %q, want %q", p, got, want)
		}
	}
	// A missing leaf under a symlinked ancestor exercises the recursive
	// canonicalizer's fallback on both sides.
	base := t.TempDir()
	link := filepath.Join(t.TempDir(), "lnk")
	MustSymlink(t, base, link)
	for _, p := range []string{filepath.Join(link, "missing"), filepath.Join(link, "a", "b")} {
		if got, want := Canonical(p), CanonicalPath(p); got != want {
			t.Fatalf("Canonical(%q) = %q, want %q", p, got, want)
		}
	}
}
