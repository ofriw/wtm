package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// fs.go — the only file with filesystem helpers.

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// canonical resolves symlinks on the deepest existing ancestor so paths from
// git, agent sessions, and the filesystem compare equal (macOS /tmp is
// /private/tmp). Non-existent tails are preserved.
func canonical(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if dir == "" || dir == p {
		return p
	}
	return filepath.Join(canonical(filepath.Clean(dir)), base)
}

// canonicalAbs evaluates symlinks on the absolute form of p.
func canonicalAbs(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return canonical(abs), nil
}

func createDstFile(dst string, mode os.FileMode) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
}

// copyFile copies one regular file, preserving its permission bits.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := createDstFile(dst, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// cloneDir copies a directory tree, preferring a reflink clone and falling
// back to a portable byte copy when the platform or filesystem cannot clone.
func cloneDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := clonePlatform(src, dst); err == nil {
		return removeExcluded(dst)
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return copyTree(src, dst)
}

// clonePlatform is best-effort: linux uses GNU `cp --reflink=auto`, darwin
// uses `cp -c` (clonefile). On any failure the caller copies byte-wise.
func clonePlatform(src, dst string) error {
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
	return childCommand("cp", args...).Run()
}

// copyTree mirrors a tree. Live daemon sidecars never cross over: a copied
// watchman/ state or daemon.log would point at the wrong root or lie about a
// running daemon.
func excluded(d fs.DirEntry) bool {
	return d.Name() == "daemon.log" || (d.IsDir() && (d.Name() == "watchman" || d.Name() == ".watchman"))
}

func removeExcluded(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !excluded(d) {
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

// copyLink replicates one symlink without dereferencing it, so dangling
// links survive and file/dir targets stay links instead of full copies.
func copyLink(src, dst string) error {
	link, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Symlink(link, dst)
}

func copyTreeDir(p, target string) error {
	mode := os.FileMode(0o755)
	if info, err := os.Stat(p); err == nil {
		mode = info.Mode().Perm()
	}
	return os.MkdirAll(target, mode)
}

func copyTreeEntry(src, dst, p string, d fs.DirEntry) error {
	rel, err := filepath.Rel(src, p)
	if err != nil || excluded(d) {
		if d.IsDir() && excluded(d) {
			return filepath.SkipDir
		}
		return err
	}
	target := filepath.Join(dst, rel)
	if d.Type()&fs.ModeSymlink != 0 {
		return copyLink(p, target)
	}
	if d.IsDir() {
		return copyTreeDir(p, target)
	}
	return copyFile(p, target)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return copyTreeEntry(src, dst, p, d)
	})
}

// maxControlFileBytes bounds settings, auth and MCP config reads. These files
// are small; the cap turns a corrupt or hostile body into a visible refusal
// instead of unbounded memory growth. The Claude account keeps its own larger
// streaming cap (subscription.go).
const maxControlFileBytes = 1 << 20

// errNotRegularFile marks a path that is not a regular file, so callers map it
// to their own verdict without matching text.
var errNotRegularFile = errors.New("not a regular file")

// openRegularFile opens path and refuses a non-regular target. The type check
// runs on the opened handle, never on the path: a path Stat leaves a window where
// the target can become a FIFO, and a blocking open then hangs status. The open
// itself is non-blocking (openNoBlock) so a FIFO handle can be inspected and
// refused. A symlink still works because the handle resolves it.
func openRegularFile(path string) (*os.File, error) {
	f, err := openNoBlock(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is %s", errNotRegularFile, path, info.Mode().Type())
	}
	return f, nil
}

// readControlFile is the guarded read for small config/control files. The type
// guard stops a symlink to a FIFO or device from hanging status; the size cap
// bounds a large regular target.
func readControlFile(path string) ([]byte, error) {
	f, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readCapped(f, path, maxControlFileBytes)
}

func readJSON(path string, v any) error {
	b, err := readControlFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// errFileTooLarge marks a readFileLimited overflow, so callers map it to their
// own size error without matching text.
var errFileTooLarge = errors.New("file exceeds size limit")

// readFileLimited reads at most limit bytes and reports overflow. Small
// control files use it so a corrupted or hostile file cannot exhaust memory.
// The open is openRegularFile: its handle check refuses a FIFO, device or
// directory before any read, so a hostile path cannot hang the caller; the
// handle still shares delete, so an atomic replace is never blocked.
func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readCapped(f, path, limit)
}

// readCapped reads at most limit bytes and reports overflow. One helper keeps the
// overflow rule shared by every capped reader.
func readCapped(r io.Reader, path string, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", errFileTooLarge, path, limit)
	}
	return data, nil
}

// writeFileAtomic replaces path by writing a sibling temp file and renaming it
// into place. A rename within one directory is atomic, so a concurrent reader
// sees either the old file or the new one, never a half-written file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	return replaceAtomicFile(f, path, data, mode)
}

func replaceAtomicFile(f *os.File, path string, data []byte, mode os.FileMode) (err error) {
	// Remove only failed writes; a successful rename releases the temp path.
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err := writeAtomicContents(f, data, mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return renameReplace(f.Name(), path)
}

func writeAtomicContents(f *os.File, data []byte, mode os.FileMode) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Chmod(mode)
}

// withFileLock runs fn while holding an exclusive OS lock on lockPath, blocking
// until it is available. The lock file is created once and never removed or
// renamed: the OS releases the lock on process exit, so a crash cannot leave a
// stale lock, and unlinking it would let the next locker lock a different inode.
func withFileLock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := lockFile(f); err != nil {
		return err
	}
	defer func() { _ = unlockFile(f) }()
	return fn()
}

// writeJSON writes indented JSON atomically. The rename protects a concurrent
// reader from a truncated body. An existing file keeps its permission bits so a
// copied 0600 secret config stays private; a new file gets world-readable 0644.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), mode)
}

// printJSON writes indented JSON to stdout (machine output contract).
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
