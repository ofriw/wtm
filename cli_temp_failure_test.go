package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAddTempRegistrationFailure(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	registry, err := tempPath()
	mustProcessError(t, err)
	mustProcessError(t, os.MkdirAll(registry, 0700))
	out, stderr, rc := runWTM(t, repo, nil, "add", "persist", "--temp", "--no-index", "--json")
	if rc != 0 || !strings.Contains(stderr, "not tracked as temp and will persist") {
		t.Fatalf("add registration failure: rc=%d stderr=%s", rc, stderr)
	}
	var res addResult
	mustProcessError(t, json.Unmarshal([]byte(out), &res))
	assertPermanentTempAdd(t, repo, res)
}

func assertPermanentTempAdd(t *testing.T, repo string, res addResult) {
	t.Helper()
	want := canonical(filepath.Join(filepath.Dir(repo), "repo-tmp-persist"))
	if res.Temp || res.TTL != "" || res.ExpiresAt != nil || res.Branch != "tmp/persist" || res.Path != want {
		t.Fatalf("registration failure result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.Path, ".git")); err != nil {
		t.Fatalf("checkout did not survive: %v", err)
	}
	if branch, err := gitCurrentBranch(res.Path); err != nil || branch != res.Branch {
		t.Fatalf("branch did not survive: branch=%q err=%v", branch, err)
	}
}

// TestNativeCorruptTempStore pins the corrupt-registry surface: read-only
// status degrades to permanent rules with a warning, while mutations fail
// closed instead of turning an unreadable record into proven absence.
func TestNativeCorruptTempStore(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	target := filepath.Join(filepath.Dir(repo), "wt-corrupt")
	gitWorktreeAdd(t, repo, target, "wt-corrupt", "main")
	p, err := tempPath()
	mustProcessError(t, err)
	mustProcessError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	mustProcessError(t, os.WriteFile(p, []byte("{broken"), 0o600))

	out, errOut, rc := runWTM(t, repo, nil, "status")
	if rc != 0 || !strings.Contains(errOut, "temp store") || !strings.Contains(out, canonical(target)) {
		t.Fatalf("status(corrupt store) = (%q, %q, rc %d)", out, errOut, rc)
	}

	_, errOut, rc = runWTM(t, repo, nil, "gc", "--path", target, "--yes")
	if rc == 0 || !strings.Contains(errOut, p) {
		t.Fatalf("gc(corrupt store) must fail closed naming %s: rc=%d stderr=%s", p, rc, errOut)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("corrupt store removed checkout: %v", err)
	}

	_, errOut, rc = runWTM(t, repo, nil, "promote", "wt-corrupt")
	if rc == 0 || !strings.Contains(errOut, p) {
		t.Fatalf("promote(corrupt store) must fail closed naming %s: rc=%d stderr=%s", p, rc, errOut)
	}
}
