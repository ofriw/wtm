package fs

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// fs.go — the only file with filesystem helpers.

func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Canonical resolves symlinks on the deepest existing ancestor so paths from
// git, agent sessions, and the filesystem compare equal (macOS /tmp is
// /private/tmp). Non-existent tails are preserved.
func Canonical(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if dir == "" || dir == p {
		return p
	}
	return filepath.Join(Canonical(filepath.Clean(dir)), base)
}

// CanonicalAbs evaluates symlinks on the absolute form of p.
func CanonicalAbs(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return Canonical(abs), nil
}

func CreateDstFile(dst string, mode os.FileMode) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
}

// CopyFile copies one regular file, preserving its permission bits.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := CreateDstFile(dst, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// CloneDir copies a directory tree, preferring a reflink clone and falling
// back to a portable byte copy when the platform or filesystem cannot clone.
func CloneDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := ClonePlatform(src, dst); err == nil {
		return RemoveExcluded(dst)
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return CopyTree(src, dst)
}

// ClonePlatform is best-effort: linux uses GNU `cp --reflink=auto`, darwin
// uses `cp -c` (clonefile). On any failure the caller copies byte-wise.
func ClonePlatform(src, dst string) error {
	if _, err := exec.LookPath("cp"); err != nil {
		return err
	}
	var args []string
	switch runtime.GOOS {
	case "linux":
		args = []string{"-R", "--reflink=auto", "--preserve=mode,timestamps", src, dst}
	case "darwin":
		args = []string{"-R", "-c", src, dst}
	default:
		return fmt.Errorf("no reflink support on %s", runtime.GOOS)
	}
	return exec.Command("cp", args...).Run()
}

// CopyTree mirrors a tree. Live daemon sidecars never cross over: a copied
// watchman/ state or daemon.log would point at the wrong root or lie about a
// running daemon.
func Excluded(d fs.DirEntry) bool {
	return d.Name() == "daemon.log" || (d.IsDir() && (d.Name() == "watchman" || d.Name() == ".watchman"))
}

func RemoveExcluded(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !Excluded(d) {
			return err
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

// CopyLink replicates one symlink without dereferencing it, so dangling
// links survive and file/dir targets stay links instead of full copies.
func CopyLink(src, dst string) error {
	link, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Symlink(link, dst)
}

func CopyTreeDir(p, target string) error {
	mode := os.FileMode(0o755)
	if info, err := os.Stat(p); err == nil {
		mode = info.Mode().Perm()
	}
	return os.MkdirAll(target, mode)
}

func CopyTreeEntry(src, dst, p string, d fs.DirEntry) error {
	rel, err := filepath.Rel(src, p)
	if err != nil || Excluded(d) {
		if d.IsDir() && Excluded(d) {
			return filepath.SkipDir
		}
		return err
	}
	target := filepath.Join(dst, rel)
	if d.Type()&fs.ModeSymlink != 0 {
		return CopyLink(p, target)
	}
	if d.IsDir() {
		return CopyTreeDir(p, target)
	}
	return CopyFile(p, target)
}

func CopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return CopyTreeEntry(src, dst, p, d)
	})
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ReadFileShared reads path while permitting a concurrent atomic replace or
// delete of that path. On Windows the reader must share delete, and the writer
// must rename with POSIX semantics: FILE_SHARE_DELETE alone does not let
// MoveFileEx replace an open destination (open_windows.go, rename_windows.go).
func ReadFileShared(path string) ([]byte, error) {
	f, err := OpenShared(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// WriteFileAtomic replaces path by writing a sibling temp file and renaming it
// into place. A rename within one directory is atomic, so a concurrent reader
// sees either the old file or the new one, never a half-written file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	return ReplaceAtomicFile(f, path, data, mode)
}

func ReplaceAtomicFile(f *os.File, path string, data []byte, mode os.FileMode) (err error) {
	// Remove only failed writes; a successful rename releases the temp path.
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err := WriteAtomicContents(f, data, mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return RenameReplace(f.Name(), path)
}

func WriteAtomicContents(f *os.File, data []byte, mode os.FileMode) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Chmod(mode)
}

// WithFileLock runs fn while holding an exclusive OS lock on lockPath, blocking
// until it is available. The lock file is created once and never removed or
// renamed: the OS releases the lock on process exit, so a crash cannot leave a
// stale lock, and unlinking it would let the next locker lock a different inode.
func WithFileLock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := LockFile(f); err != nil {
		return err
	}
	defer func() { _ = UnlockFile(f) }()
	return fn()
}

// WriteJSON writes indented JSON with world-readable 0644.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// PrintJSON writes indented JSON to stdout (machine output contract).
func PrintJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
