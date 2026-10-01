package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cli_native_test.go — Tier 3: exec the real wtm binary on the host OS. These
// tests never assert on mode bits, byte output or path separators; symlink
// behavior is exercised elsewhere and skipped when unavailable.

func nativeRepo(t *testing.T) string {
	t.Helper()
	repo := initRepo(t, "main")
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n", 0o644)
	gitCommit(t, repo, "seed", "README.md")
	return repo
}

func TestNativeExitCodes(t *testing.T) {
	sandbox(t)
	outside := t.TempDir()
	repo := nativeRepo(t)

	cases := []struct {
		name string
		dir  string
		args []string
		want int
	}{
		{"help", outside, []string{"help"}, 0},
		{"h flag", outside, []string{"-h"}, 0},
		{"unknown command", outside, []string{"bogus"}, 2},
		{"status outside repo", outside, []string{"status"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, rc := runWTM(t, tc.dir, nil, tc.args...)
			if rc != tc.want {
				t.Fatalf("wtm %v rc = %d, want %d", tc.args, rc, tc.want)
			}
		})
	}

	t.Run("invalid config", func(t *testing.T) {
		setTTL(t, "0d")
		_, _, rc := runWTM(t, repo, nil, "config")
		if rc != 1 {
			t.Fatalf("config with invalid TTL rc = %d, want 1", rc)
		}
	})
}

func TestNativeStatusJSON(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	out, errOut, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status rc = %d, stderr = %s", rc, errOut)
	}
	var wts []struct {
		Path     string  `json:"path"`
		Branch   string  `json:"branch"`
		Main     bool    `json:"main"`
		LastUsed *string `json:"lastUsed"`
	}
	if err := json.Unmarshal([]byte(out), &wts); err != nil {
		t.Fatalf("status JSON invalid: %v\n%s", err, out)
	}
	if len(wts) != 1 || !wts[0].Main || wts[0].Branch != "main" {
		t.Fatalf("status JSON = %+v, want one main worktree on branch main", wts)
	}
	if canonical(wts[0].Path) != canonical(repo) {
		t.Fatalf("status path = %q, want %q", wts[0].Path, canonical(repo))
	}
}

func TestNativeAddNoIndex(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := filepath.Join(filepath.Dir(repo), "wt-native")
	out, errOut, rc := runWTM(t, repo, nil, "add", target, "--no-index", "--json")
	if rc != 0 {
		t.Fatalf("add rc = %d, stderr = %s", rc, errOut)
	}
	var res addResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("add JSON invalid: %v\n%s", err, out)
	}
	if res.Indexed || res.Branch != "wt-native" || res.Path != canonical(target) {
		t.Fatalf("add result = %+v", res)
	}
	info, err := os.Stat(filepath.Join(target, ".git"))
	if err != nil || info.IsDir() {
		t.Fatalf("linked worktree .git must be a file: info=%v err=%v", info, err)
	}
}

func TestNativeConfigRoundTrip(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	if _, errOut, rc := runWTM(t, repo, nil, "config", "set", "unusedTTL", "7d"); rc != 0 {
		t.Fatalf("config set rc = %d, stderr = %s", rc, errOut)
	}
	out, errOut, rc := runWTM(t, repo, nil, "--json", "config")
	if rc != 0 {
		t.Fatalf("config rc = %d, stderr = %s", rc, errOut)
	}
	var got struct {
		Path      string `json:"path"`
		UnusedTTL string `json:"unusedTTL"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("config JSON invalid: %v\n%s", err, out)
	}
	if got.UnusedTTL != "7d" || got.Path != settingsFile(t) {
		t.Fatalf("config = %+v, want 7d at %s", got, settingsFile(t))
	}
}

func TestNativeGC(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)

	out, _, rc := runWTM(t, repo, nil, "gc", "--all", "--yes")
	if rc != 0 || !strings.Contains(out, "nothing to do") {
		t.Fatalf("empty gc = (%q, rc %d), want nothing to do", out, rc)
	}

	target := filepath.Join(filepath.Dir(repo), "wt-gc")
	gitWorktreeAdd(t, repo, target, "wt-gc", "main")
	if _, err := git(target, "log", "-1", "--format=%ct"); err != nil {
		t.Fatalf("worktree not usable: %v", err)
	}
	out, errOut, rc := runWTM(t, repo, nil, "gc", "--path", target, "--yes")
	if rc != 0 {
		t.Fatalf("gc --path rc = %d, stderr = %s", rc, errOut)
	}
	if !strings.Contains(out, "removed "+canonical(target)) {
		t.Fatalf("gc output = %q, want removal of %s", out, canonical(target))
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after gc", target)
	}
}

// TestNativeGCJSON pins the binary-level --json contract for gc: exit 0, and
// a report whose slices serialize as [] (never null) after a real removal.
func TestNativeGCJSON(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := filepath.Join(filepath.Dir(repo), "wt-json")
	gitWorktreeAdd(t, repo, target, "wt-json", "main")
	out, errOut, rc := runWTM(t, repo, nil, "gc", "--path", target, "--yes", "--json")
	if rc != 0 {
		t.Fatalf("gc --json rc = %d, stderr = %s", rc, errOut)
	}
	var res gcResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("gc JSON invalid: %v\n%s", err, out)
	}
	if len(res.Removed) != 1 || res.Removed[0] != canonical(target) {
		t.Fatalf("Removed = %v, want [%s]", res.Removed, canonical(target))
	}
	if !res.Pruned || res.SessionsPurged != 0 || res.KeptSessions {
		t.Fatalf("report = %+v, want pruned, 0 purged, sessions not kept", res)
	}
	if strings.Contains(out, "null") {
		t.Fatalf("gc JSON must serialize empty slices as [], got %s", out)
	}
}

// TestNativeGCWarnsDirty pins the forced-removal disclosure: worktreeRemove is
// always --force, so a dirty worktree removed via --path must warn on stderr
// (stdout stays machine-clean) while a clean removal stays silent.
func TestNativeGCWarnsDirty(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := filepath.Join(filepath.Dir(repo), "wt-dirty")
	gitWorktreeAdd(t, repo, target, "wt-dirty", "main")
	writeFile(t, filepath.Join(target, "README.md"), "dirty-edit\n", 0o644)
	out, errOut, rc := runWTM(t, repo, nil, "gc", "--path", target, "--yes")
	if rc != 0 {
		t.Fatalf("gc rc = %d, stderr = %s", rc, errOut)
	}
	if !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "uncommitted changes") {
		t.Fatalf("dirty removal must warn on stderr, got %q", errOut)
	}
	if strings.Contains(out, "warning") {
		t.Fatalf("warning must stay off stdout, got %q", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after gc", target)
	}
}

func TestNativeRootFlag(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	outside := t.TempDir()
	out, errOut, rc := runWTM(t, outside, nil, "--root", repo, "status", "--json")
	if rc != 0 {
		t.Fatalf("status --root rc = %d, stderr = %s", rc, errOut)
	}
	var wts []struct {
		Path string `json:"path"`
		Main bool   `json:"main"`
	}
	if err := json.Unmarshal([]byte(out), &wts); err != nil {
		t.Fatalf("status JSON invalid: %v\n%s", err, out)
	}
	if len(wts) != 1 || !wts[0].Main || canonical(wts[0].Path) != canonical(repo) {
		t.Fatalf("--root status = %+v, want main %s", wts, canonical(repo))
	}
}

// TestNativeGCDoesNotSelectFresh guards the binary's default TTL path: a
// just-created worktree (recent .git mtime) is never collected by --all.
func TestNativeGCDoesNotSelectFresh(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := filepath.Join(filepath.Dir(repo), "wt-fresh")
	gitWorktreeAdd(t, repo, target, "wt-fresh", "main")
	if err := os.Chtimes(target, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	out, _, rc := runWTM(t, repo, nil, "gc", "--all", "--yes")
	if rc != 0 || !strings.Contains(out, "nothing to do") {
		t.Fatalf("gc --all on fresh worktree = (%q, rc %d), want nothing to do", out, rc)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("fresh worktree was collected: %v", err)
	}
}
