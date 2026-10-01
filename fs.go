package main

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

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// canonical resolves symlinks on the deepest existing ancestor so paths from
// git, Pi sessions, and the filesystem compare equal (macOS /tmp is
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
	return exec.Command("cp", args...).Run()
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

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// printJSON writes indented JSON to stdout (machine output contract).
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
