package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"wtm/internal/fs"
	. "wtm/internal/testutil"
)

// chunkhound_test.go — Tier-1 contracts for chunkhound.go. RunIndex shells out
// to the real chunkhound binary; apart from the timeout test below (which plants
// a hung stub on PATH) it is deliberately never called here.

func TestHasChunkHoundWorkspace(t *testing.T) {
	t.Run("config only", func(t *testing.T) {
		dir := t.TempDir()
		WriteFile(t, filepath.Join(dir, ChunkhoundConfigFile), "{}", 0o644)
		if !HasChunkHoundWorkspace(dir) {
			t.Fatal("config alone must count as a workspace")
		}
	})

	t.Run("db only", func(t *testing.T) {
		dir := t.TempDir()
		WriteFile(t, ChunkhoundDBPath(dir), "DB", 0o644)
		if !HasChunkHoundWorkspace(dir) {
			t.Fatal("db alone must count as a workspace")
		}
	})

	t.Run("empty db dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(ChunkhoundDBDir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if HasChunkHoundWorkspace(dir) {
			t.Fatal("empty db dir is not a workspace")
		}
	})

	t.Run("neither", func(t *testing.T) {
		if HasChunkHoundWorkspace(t.TempDir()) {
			t.Fatal("empty dir is not a workspace")
		}
	})
}

func TestRewriteRootGuard(t *testing.T) {
	t.Run("rewrites Canonical slash form and preserves unknown keys", func(t *testing.T) {
		dst := t.TempDir()
		guard := filepath.Join(ChunkhoundDBDir(dst), RootGuardName)
		WriteFile(t, guard, `{"indexed_root_path":"/old","extra":1}`, 0o644)
		if err := RewriteRootGuard(dst); err != nil {
			t.Fatalf("RewriteRootGuard: %v", err)
		}
		var m map[string]json.RawMessage
		if err := fs.ReadJSON(guard, &m); err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(filepath.ToSlash(CanonicalPath(dst)))
		if string(m["indexed_root_path"]) != string(want) {
			t.Fatalf("indexed_root_path = %s, want %s", m["indexed_root_path"], want)
		}
		if string(m["extra"]) != "1" {
			t.Fatalf("unknown key lost: %s", m["extra"])
		}
	})

	t.Run("missing guard is a no-op", func(t *testing.T) {
		if err := RewriteRootGuard(t.TempDir()); err != nil {
			t.Fatalf("missing guard = %v, want nil", err)
		}
	})

	t.Run("malformed guard errors naming the path", func(t *testing.T) {
		dst := t.TempDir()
		guard := filepath.Join(ChunkhoundDBDir(dst), RootGuardName)
		WriteFile(t, guard, "{not json", 0o644)
		err := RewriteRootGuard(dst)
		if err == nil {
			t.Fatal("malformed guard must error")
		}
		if !strings.Contains(err.Error(), guard) {
			t.Fatalf("error %q must name guard path %q", err, guard)
		}
	})
}

func chJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// chRunPatch writes body as the dst config, runs PatchDatabasePath and returns
// the resulting config exactly as it landed on disk.
func chRunPatch(t *testing.T, srcRoot, dstRoot, body string) string {
	t.Helper()
	WriteFile(t, filepath.Join(dstRoot, ChunkhoundConfigFile), body, 0o644)
	if err := PatchDatabasePath(srcRoot, dstRoot); err != nil {
		t.Fatalf("PatchDatabasePath: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dstRoot, ChunkhoundConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func chDBPath(t *testing.T, config string) string {
	t.Helper()
	var c struct {
		Database struct {
			Path string `json:"path"`
		} `json:"database"`
	}
	if err := json.Unmarshal([]byte(config), &c); err != nil {
		t.Fatal(err)
	}
	return c.Database.Path
}

func TestPatchDatabasePath(t *testing.T) {
	rawSrc := t.TempDir()
	rawDst := t.TempDir()
	// Canonical roots so the prefix check works on hosts where t.TempDir() is
	// symlinked (/var -> /private/var on macOS).
	srcRoot := CanonicalPath(rawSrc)
	dstRoot := CanonicalPath(rawDst)
	inside := filepath.Join(rawSrc, "sub", "db")
	outside := filepath.Join(t.TempDir(), "elsewhere")
	wantInside := filepath.ToSlash(filepath.Join(dstRoot, "sub", "db"))

	t.Run("absolute inside srcRoot rewritten and slash-normalized", func(t *testing.T) {
		body := `{"database":{"path":` + chJSONString(t, inside) + `}}`
		got := chDBPath(t, chRunPatch(t, srcRoot, dstRoot, body))
		if got != wantInside {
			t.Fatalf("path = %q, want %q", got, wantInside)
		}
		if strings.Contains(got, `\`) {
			t.Fatalf("path not slash-normalized: %q", got)
		}
	})

	t.Run("absolute outside srcRoot verbatim", func(t *testing.T) {
		body := `{"database":{"path":` + chJSONString(t, outside) + `}}`
		if got := chDBPath(t, chRunPatch(t, srcRoot, dstRoot, body)); got != outside {
			t.Fatalf("path = %q, want %q", got, outside)
		}
	})

	t.Run("relative path verbatim", func(t *testing.T) {
		body := `{"database":{"path":"rel/db"}}`
		if got := chDBPath(t, chRunPatch(t, srcRoot, dstRoot, body)); got != "rel/db" {
			t.Fatalf("path = %q, want rel/db", got)
		}
	})

	t.Run("missing database key is a no-op", func(t *testing.T) {
		if body := `{"other":1}`; chRunPatch(t, srcRoot, dstRoot, body) != body {
			t.Fatalf("config changed for missing database key")
		}
	})

	t.Run("non-object database is a no-op", func(t *testing.T) {
		if body := `{"database":"oops"}`; chRunPatch(t, srcRoot, dstRoot, body) != body {
			t.Fatalf("config changed for non-object database")
		}
	})

	t.Run("non-string path is a no-op", func(t *testing.T) {
		if body := `{"database":{"path":7}}`; chRunPatch(t, srcRoot, dstRoot, body) != body {
			t.Fatalf("config changed for non-string path")
		}
	})

	// Corrupt configs warn and stay verbatim: an unparseable file or database
	// object is treated as absent (no rewrite), never a healthy patched claim.
	t.Run("corrupt config or database object left verbatim", func(t *testing.T) {
		if body := `{not json`; chRunPatch(t, srcRoot, dstRoot, body) != body {
			t.Fatalf("corrupt config changed: %s", body)
		}
		if body := `{"database":[1,2]}`; chRunPatch(t, srcRoot, dstRoot, body) != body {
			t.Fatalf("config with unparseable database object changed: %s", body)
		}
	})

	t.Run("unknown keys preserved", func(t *testing.T) {
		body := `{"database":{"path":` + chJSONString(t, inside) + `,"mode":"ro"},"extra":"keep"}`
		got := chRunPatch(t, srcRoot, dstRoot, body)
		if p := chDBPath(t, got); p != wantInside {
			t.Fatalf("path = %q, want %q", p, wantInside)
		}
		if !strings.Contains(got, `"mode"`) || !strings.Contains(got, `"extra"`) {
			t.Fatalf("unknown keys lost: %s", got)
		}
	})

	t.Run("symlinked prefix still rewritten", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "srclink")
		MustSymlink(t, rawSrc, link)
		body := `{"database":{"path":` + chJSONString(t, filepath.Join(link, "sub", "db")) + `}}`
		if got := chDBPath(t, chRunPatch(t, srcRoot, dstRoot, body)); got != wantInside {
			t.Fatalf("path = %q, want %q", got, wantInside)
		}
	})
}

func TestCopyChunkHound(t *testing.T) {
	t.Run("config only", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		WriteFile(t, filepath.Join(src, ChunkhoundConfigFile), `{"a":1}`, 0o644)
		if err := CopyChunkHound(src, dst); err != nil {
			t.Fatalf("CopyChunkHound: %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(dst, ChunkhoundConfigFile)); string(b) != `{"a":1}` {
			t.Fatalf("config = %q", b)
		}
		if fs.Exists(ChunkhoundDBDir(dst)) {
			t.Fatal("db dir must not be created")
		}
	})

	t.Run("db only", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		WriteFile(t, ChunkhoundDBPath(src), "DB", 0o644)
		if err := CopyChunkHound(src, dst); err != nil {
			t.Fatalf("CopyChunkHound: %v", err)
		}
		if b, _ := os.ReadFile(ChunkhoundDBPath(dst)); string(b) != "DB" {
			t.Fatalf("db = %q, want DB", b)
		}
		if fs.Exists(filepath.Join(dst, ChunkhoundConfigFile)) {
			t.Fatal("config must not be created")
		}
	})

	t.Run("both", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		WriteFile(t, filepath.Join(src, ChunkhoundConfigFile), "CFG", 0o644)
		WriteFile(t, ChunkhoundDBPath(src), "DB", 0o644)
		if err := CopyChunkHound(src, dst); err != nil {
			t.Fatalf("CopyChunkHound: %v", err)
		}
		if !fs.Exists(filepath.Join(dst, ChunkhoundConfigFile)) || !fs.Exists(ChunkhoundDBPath(dst)) {
			t.Fatal("both artifacts must land")
		}
	})

	t.Run("neither", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		if err := CopyChunkHound(src, dst); err != nil {
			t.Fatalf("CopyChunkHound: %v", err)
		}
		if fs.Exists(filepath.Join(dst, ChunkhoundConfigFile)) || fs.Exists(ChunkhoundDBDir(dst)) {
			t.Fatal("nothing must land")
		}
	})

	t.Run("empty db dir", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(ChunkhoundDBDir(src), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := CopyChunkHound(src, dst); err != nil {
			t.Fatalf("CopyChunkHound: %v", err)
		}
		if !fs.Exists(ChunkhoundDBDir(dst)) {
			t.Fatal("copy rule must preserve an existing db dir")
		}
	})
}

func TestChunkhoundPaths(t *testing.T) {
	root := filepath.Join("some", "root")
	if got, want := ChunkhoundDBDir(root), filepath.Join(root, chunkhoundDir, chunkhoundDBDirName); got != want {
		t.Fatalf("ChunkhoundDBDir = %q, want %q", got, want)
	}
	if got, want := ChunkhoundDBPath(root), filepath.Join(ChunkhoundDBDir(root), "chunks.db"); got != want {
		t.Fatalf("ChunkhoundDBPath = %q, want %q", got, want)
	}
}

// TestRunIndexTimeoutMessage pins that when the indexer outlives its bound, the
// error must name the timeout. Go discards ctx.Err() once the killed process's
// Wait returns, so without the explicit check the failure reads as
// "signal: killed" and a bound masquerades as a crash.
func TestRunIndexTimeoutMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell stub for a hung chunkhound")
	}
	dir := t.TempDir()
	// An absolute /bin/sleep keeps the stub independent of PATH, which only has
	// to resolve the chunkhound name.
	WriteFile(t, filepath.Join(dir, "chunkhound"), "#!/bin/sh\nexec /bin/sleep 30\n", 0o755)
	t.Setenv("PATH", dir)
	old := IndexTimeout
	IndexTimeout = 50 * time.Millisecond
	t.Cleanup(func() { IndexTimeout = old })

	err := RunIndex(t.TempDir())
	if err == nil {
		t.Fatal("RunIndex succeeded, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error %q must name the timeout", err)
	}
	if strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("error %q leaked the raw kill status", err)
	}
}
