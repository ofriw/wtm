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
