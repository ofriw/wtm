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
		{"invalid color after subcommand", repo, []string{"status", "--color", "bogus"}, 2},
		{"invalid progress after subcommand", repo, []string{"status", "--progress", "bogus"}, 2},
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

// TestNativeNoTerminalQueries pins the input-disabled contract on the host OS:
// forced progress must not write a reply-expecting terminal query, because the
// terminal's answer would be left in the tty and echoed by the shell after wtm
// exits (charmbracelet/bubbletea#1590).
func TestNativeNoTerminalQueries(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	_, errOut, rc := runWTM(t, repo, nil, "status", "--progress=always")
	if rc != 0 {
		t.Fatalf("status rc = %d, stderr = %s", rc, errOut)
	}
	assertNoTerminalQueries(t, errOut)
}

func TestNativeAddNoIndex(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := canonical(filepath.Join(filepath.Dir(repo), "repo-wt-native"))
	out, errOut, rc := runWTM(t, repo, nil, "add", "wt-native", "--no-index", "--json")
	if rc != 0 {
		t.Fatalf("add rc = %d, stderr = %s", rc, errOut)
	}
	var res addResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("add JSON invalid: %v\n%s", err, out)
	}
	if res.Indexed || res.Branch != "wt-native" || res.Path != target {
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

// TestNativeDeleteSmoke pins that wtm delete removes the worktree and its local
// branch via external binary execution.
func TestNativeDeleteSmoke(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := filepath.Join(filepath.Dir(repo), "wt-del")
	gitWorktreeAdd(t, repo, target, "feature/to-del", "main")
	out, errOut, rc := runWTM(t, repo, nil, "delete", "feature/to-del", "--yes", "--json")
	if rc != 0 {
		t.Fatalf("delete --json rc = %d, stderr = %s", rc, errOut)
	}
	var res gcResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("delete JSON invalid: %v\n%s", err, out)
	}
	if len(res.Removed) != 1 || res.Removed[0] != canonical(target) {
		t.Fatalf("Removed = %v, want [%s]", res.Removed, canonical(target))
	}
	if res.DeletedBranch != "feature/to-del" {
		t.Fatalf("DeletedBranch = %q, want feature/to-del", res.DeletedBranch)
	}
	if exists(target) {
		t.Fatalf("worktree %s survived delete", target)
	}
	if refExists(repo, "refs/heads/feature/to-del") {
		t.Fatal("local branch feature/to-del survived delete")
	}
}

// TestNativeGCKeepRemote pins that --keep-remote parses and that a branch
// with no upstream still removes cleanly (decision 3). KeptRemote reports an
// actual keep, so with no remote upstream it stays false.
func TestNativeGCKeepRemote(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := filepath.Join(filepath.Dir(repo), "wt-keepremote")
	gitWorktreeAdd(t, repo, target, "wt-keepremote", "main")
	out, errOut, rc := runWTM(t, repo, nil, "gc", "--path", target, "--yes", "--keep-remote", "--json")
	if rc != 0 {
		t.Fatalf("gc --keep-remote rc = %d, stderr = %s", rc, errOut)
	}
	var res gcResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("gc JSON invalid: %v\n%s", err, out)
	}
	if res.KeptRemote || len(res.RemoteDeleted) != 0 {
		t.Fatalf("report = %+v, want keptRemote false and no remote deletions", res)
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

// TestNativeAddInvalidTTL pins the usage exit for a bad --ttl: an unparsable
// window is a command-line error (rc 2), never a silent default.
func TestNativeAddInvalidTTL(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	_, errOut, rc := runWTM(t, repo, nil, "add", "badttl", "--ttl", "nope")
	if rc != 2 || !strings.Contains(errOut, "ttl") {
		t.Fatalf("add --ttl nope = (rc %d, stderr %q), want rc 2 mentioning ttl", rc, errOut)
	}
}

// TestNativeAddTTLAccepted drives every accepted --ttl form through the real
// binary and pins the JSON deadline against the persisted record.
func TestNativeAddTTLAccepted(t *testing.T) {
	// Go durations accept a leading +; pin the real contract, not a stricter one.
	cases := []struct {
		name  string
		ttl   string
		delta time.Duration
	}{
		{"minutes", "30m", 30 * time.Minute},
		{"hours", "1h", time.Hour},
		{"compound", "1h30m", 90 * time.Minute},
		{"days", "7d", 7 * 24 * time.Hour},
		{"fractional", "1.5s", 1500 * time.Millisecond},
		{"signedplus", "+1h", time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			repo := nativeRepo(t)
			out, errOut, rc := runWTM(t, repo, nil, "add", tc.name, "--ttl", tc.ttl, "--no-index", "--json")
			if rc != 0 {
				t.Fatalf("add --ttl %s rc = %d, stderr = %s", tc.ttl, rc, errOut)
			}
			assertNativeTempDeadline(t, decodeAddResult(t, out), "tmp/"+tc.name, tc.delta)
		})
	}
}

// TestNativeAddTTLRejected pins the usage exit for every malformed window.
func TestNativeAddTTLRejected(t *testing.T) {
	cases := []struct{ name, ttl string }{
		{"zero", "0s"},
		{"negative", "-1h"},
		{"trailing space", "1h "},
		{"garbage", "abc"},
		{"over max", "999999d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			repo := nativeRepo(t)
			_, errOut, rc := runWTM(t, repo, nil, "add", tc.name, "--ttl", tc.ttl, "--no-index")
			if rc == 0 {
				t.Fatalf("add --ttl %q rc = 0, want non-zero (stderr %s)", tc.ttl, errOut)
			}
		})
	}
}

// TestNativeAddTTLImpliesTemp pins that --ttl alone creates a tmp/ branch.
func TestNativeAddTTLImpliesTemp(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	out, errOut, rc := runWTM(t, repo, nil, "add", "implied", "--ttl", "1h", "--no-index", "--json")
	if rc != 0 {
		t.Fatalf("add --ttl 1h rc = %d, stderr = %s", rc, errOut)
	}
	assertNativeTempDeadline(t, decodeAddResult(t, out), "tmp/implied", time.Hour)
}

func decodeAddResult(t *testing.T, out string) addResult {
	t.Helper()
	var res addResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("add JSON invalid: %v\n%s", err, out)
	}
	return res
}

// assertNativeTempDeadline pins the user-facing temp contract: a tmp/ branch,
// the temp flag, a parseable TTL, and an expiresAt derived from the record.
func assertNativeTempDeadline(t *testing.T, res addResult, wantBranch string, delta time.Duration) {
	t.Helper()
	if !res.Temp || res.Branch != wantBranch {
		t.Fatalf("add result = %+v, want temp %s", res, wantBranch)
	}
	if got, err := time.ParseDuration(res.TTL); err != nil || got != delta {
		t.Fatalf("ttl = %q, want %s (err %v)", res.TTL, delta, err)
	}
	rec, ok := mustReadTempStore(t)[canonical(res.Path)]
	if !ok {
		t.Fatalf("no temp record for %s", res.Path)
	}
	wantExpiry := rec.CreatedAt.Add(delta).UTC().Format(time.RFC3339Nano)
	if res.ExpiresAt == nil || *res.ExpiresAt != wantExpiry {
		t.Fatalf("expiresAt = %v, want %s", res.ExpiresAt, wantExpiry)
	}
}

// TestNativeAddTempJSON pins the binary-level temp add: the tmp/ branch, the
// temp/expiresAt JSON fields, and the persisted record.
func TestNativeAddTempJSON(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := canonical(filepath.Join(filepath.Dir(repo), "repo-tmp-native"))
	out, errOut, rc := runWTM(t, repo, nil, "add", "native", "--temp", "--no-index", "--json")
	if rc != 0 {
		t.Fatalf("add --temp rc = %d, stderr = %s", rc, errOut)
	}
	res := decodeAddResult(t, out)
	if !res.Temp || res.Branch != "tmp/native" || res.Path != target {
		t.Fatalf("add result = %+v, want tmp/native at %s", res, target)
	}
	if res.ExpiresAt == nil || *res.ExpiresAt == "" {
		t.Fatalf("temp add must report expiresAt: %+v", res)
	}
	rec, ok := mustReadTempStore(t)[target]
	if !ok || rec.Branch != "tmp/native" {
		t.Fatalf("temp record = %+v, want a record for %s", rec, target)
	}
}
