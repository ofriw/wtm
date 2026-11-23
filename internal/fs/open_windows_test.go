//go:build windows

package fs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestOpenSharedAllowsReplaceAndRemove pins the invariant behind every store
// reader: a held reader handle must not block the writer's atomic replace or
// delete, or Windows fails them with ERROR_ACCESS_DENIED. It also pins snapshot
// semantics: the handle keeps reading the replaced file, never a half-written mix.
func TestOpenSharedAllowsReplaceAndRemove(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.json")
	mustWriteWindowsStore(t, p, `{"v":"old"}`)
	f := openWindowsReader(t, p, OpenShared)
	mustWriteWindowsStore(t, p, `{"v":"new"}`)
	assertWindowsSnapshots(t, f, p, `{"v":"old"}`, `{"v":"new"}`)

	assertWindowsRemoveSnapshot(t, p, `{"v":"new"}`)
}

func assertWindowsRemoveSnapshot(t *testing.T, path, want string) {
	t.Helper()
	current := openWindowsReader(t, path, OpenShared)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove while held open: %v", err)
	}
	assertWindowsReaderContents(t, current, want)
	// DeleteFile completes deletion only after the last held reader closes.
	if err := current.Close(); err != nil {
		t.Fatalf("close removed reader: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed pathname remains accessible: %v", err)
	}
}

// TestExtendedPath pins long paths get the extended prefix, including UNC form.
func TestExtendedPath(t *testing.T) {
	longDrive := `C:\` + strings.Repeat("dir\\", 60) + "store.json"
	if got := ExtendedPath(longDrive); !strings.HasPrefix(got, `\\?\C:\`) {
		t.Fatalf("drive path = %q, want \\\\?\\C:\\ prefix", got)
	}

	longUNC := `\\server\share\` + strings.Repeat("dir\\", 60) + "store.json"
	if got := ExtendedPath(longUNC); !strings.HasPrefix(got, `\\?\UNC\server\share\`) {
		t.Fatalf("UNC path = %q, want \\\\?\\UNC\\ prefix", got)
	}

	prefixed := `\\?\C:\already-extended`
	if got := ExtendedPath(prefixed); got != prefixed {
		t.Fatalf("prefixed path = %q, want unchanged", got)
	}
}

// TestReadFileSharedLongPath pins end-to-end long-path: atomic write, shared read.
func TestReadFileSharedLongPath(t *testing.T) {
	p := longWindowsStorePath(t, "store.json")

	mustWriteWindowsStore(t, p, "long-path")
	assertWindowsStoreContents(t, p, "long-path")
}

// A long Unicode target checks UTF-16 serialization through the native API.
// The held handle must retain the old snapshot after POSIX replacement.
func TestWriteFileAtomicLongPathOverSharedHandle(t *testing.T) {
	p := longWindowsStorePath(t, "store-世界-😀.json")
	mustWriteWindowsStore(t, p, "old")
	f := openWindowsReader(t, p, OpenShared)
	mustWriteWindowsStore(t, p, "new")
	assertWindowsSnapshots(t, f, p, "old", "new")
}

// A reader without delete sharing must prevent replacement without data loss.
// Closing it must allow the next write; failed writes must leave no temp file.
func TestWriteFileAtomicBlockedReaderPreservesDestination(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.json")
	mustWriteWindowsStore(t, p, "old")
	f := openWindowsReader(t, p, os.Open)
	if err := WriteFileAtomic(p, []byte("new"), 0o600); err == nil {
		t.Fatal("atomic replacement succeeded over a reader without delete sharing")
	}
	assertWindowsSnapshots(t, f, p, "old", "old")
	assertOnlyWindowsStore(t, p)
	if err := f.Close(); err != nil {
		t.Fatalf("close blocking reader: %v", err)
	}
	mustWriteWindowsStore(t, p, "new")
	assertWindowsStoreContents(t, p, "new")
	assertOnlyWindowsStore(t, p)
}

func TestIsOpenHandleReplaceErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"access denied", windows.ERROR_ACCESS_DENIED, true},
		{"sharing violation", windows.ERROR_SHARING_VIOLATION, true},
		{"not found", windows.ERROR_FILE_NOT_FOUND, false},
		{"other", errors.New("boom"), false},
		{"wrapped link", &os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_ACCESS_DENIED}, true},
	}
	for _, tc := range cases {
		if got := IsOpenHandleReplaceErr(tc.err); got != tc.want {
			t.Errorf("%s: fs.IsOpenHandleReplaceErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Both attempts fail: os.Rename refuses the held destination and the POSIX
// fallback cannot replace over a handle without delete sharing. The join
// preserves both causes and neither side loses data.
func TestRenameReplaceJoinsBothCauses(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "store.json")
	mustWriteWindowsStore(t, dest, "old")
	openWindowsReader(t, dest, os.Open)
	src := mustTempRenameSrc(t, "new")
	want := blockedRenameCauses(t, src, dest)
	err := RenameReplace(src, dest)
	if err == nil {
		t.Fatal("fs.RenameReplace succeeded over a reader without delete sharing")
	}
	assertJoinedPair(t, err, want)
	assertWindowsStoreContents(t, dest, "old")
	assertWindowsStoreContents(t, src, "new")
}

// Success after premise failure proves the POSIX fallback ran: MoveFileEx
// alone refuses the held destination, so only fs.PosixRename can complete it.
func TestRenameReplaceUsesPosixFallbackOverSharedHandle(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "store.json")
	mustWriteWindowsStore(t, dest, "old")
	held := openWindowsReader(t, dest, OpenShared)
	premise := mustTempRenameSrc(t, "new")
	if err := os.Rename(premise, dest); err == nil {
		t.Skip("premise failed: os.Rename replaced a held destination, env does not enforce share semantics")
	}
	if err := os.Remove(premise); err != nil {
		t.Fatalf("remove premise source: %v", err)
	}
	src := mustTempRenameSrc(t, "new")
	if err := RenameReplace(src, dest); err != nil {
		t.Fatalf("fs.RenameReplace over shared handle: %v", err)
	}
	assertWindowsSnapshots(t, held, dest, "old", "new")
}

// Happy path without an open handle: MoveFileEx succeeds; the source is gone.
func TestRenameReplaceHappyPathWithoutOpenHandle(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "store.json")
	mustWriteWindowsStore(t, dest, "old")
	src := mustTempRenameSrc(t, "new")
	if err := RenameReplace(src, dest); err != nil {
		t.Fatalf("fs.RenameReplace without open handle: %v", err)
	}
	assertWindowsStoreContents(t, dest, "new")
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still exists after replace: %v", err)
	}
}

func mustTempRenameSrc(t *testing.T, contents string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src.json")
	if err := os.WriteFile(src, []byte(contents), 0o600); err != nil {
		t.Fatalf("write rename source: %v", err)
	}
	return src
}

// Observe native failures rather than hardcode codes that can vary by Windows version.
func blockedRenameCauses(t *testing.T, src, dest string) []error {
	t.Helper()
	var causes []error
	for _, rename := range []func(string, string) error{os.Rename, PosixRename} {
		err := rename(src, dest)
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("expected blocked rename, got %v", err)
		}
		causes = append(causes, err)
	}
	return causes
}

func assertJoinedPair(t *testing.T, err error, want []error) {
	t.Helper()
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("error does not join causes: %T (%v)", err, err)
	}
	causes := joined.Unwrap()
	if len(causes) != len(want) {
		t.Fatalf("joined causes = %d, want %d (%v)", len(causes), len(want), err)
	}
	for i, expected := range want {
		assertRenameCause(t, causes[i], expected)
	}
}

func assertRenameCause(t *testing.T, got, want error) {
	t.Helper()
	var actual, expected *os.LinkError
	if !errors.As(got, &actual) || !errors.As(want, &expected) {
		t.Fatalf("rename causes lack path context: got %v, want %v", got, want)
	}
	if actual.Op != expected.Op || actual.Old != expected.Old || actual.New != expected.New || !errors.Is(got, expected.Err) {
		t.Fatalf("rename cause = %v, want code and path context of %v", got, want)
	}
}

func longWindowsStorePath(t *testing.T, name string) string {
	t.Helper()
	deep := t.TempDir()
	for len(filepath.Join(deep, name)) < 300 {
		deep = filepath.Join(deep, "padding-directory-name-xxxxxxxxxx")
	}
	return filepath.Join(deep, name)
}

func mustWriteWindowsStore(t *testing.T, path, contents string) {
	t.Helper()
	if err := WriteFileAtomic(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("fs.WriteFileAtomic %q: %v", path, err)
	}
}

func openWindowsReader(t *testing.T, path string, open func(string) (*os.File, error)) *os.File {
	t.Helper()
	f, err := open(path)
	if err != nil {
		t.Fatalf("open reader %q: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func assertWindowsSnapshots(t *testing.T, f *os.File, path, held, fresh string) {
	t.Helper()
	assertWindowsReaderContents(t, f, held)
	assertWindowsStoreContents(t, path, fresh)
}

func assertWindowsReaderContents(t *testing.T, f *os.File, want string) {
	t.Helper()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read held handle: %v", err)
	}
	if string(got) != want {
		t.Fatalf("held handle = %q, want %q", got, want)
	}
}

func assertWindowsStoreContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := ReadFileShared(path)
	if err != nil {
		t.Fatalf("fs.ReadFileShared %q: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func assertOnlyWindowsStore(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read store directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("store directory = %v, want only %q", entries, filepath.Base(path))
	}
}
