package main

import (
	"path/filepath"
	"testing"
)

// Permanent cleanup consumes discovery's ownership index without a second scan.
func TestGCCleanupReusesDiscoverySessionIndex(t *testing.T) {
	sandbox(t)
	repo, linked, pi := gcTestLinkedWorktree(t)
	claude := mkClaudeSession(t, linked, "owned", tempNow)
	w := discovered(t, repo, linked)
	idx := mustIndexSessions(t)
	unknown := filepath.Join(filepath.Dir(claude), "unverified.jsonl")
	writeFile(t, unknown, "{broken\n", 0o600)
	res, err := gcCleanup(repo, []worktree{w}, remotePlan{keep: true}, false, idx, gcMode{}, tempNow, nil)
	if err != nil || res.SessionsPurged != 2 || len(res.Removed) != 1 {
		t.Fatalf("cleanup = %+v, %v; want checkout and two owned sessions removed", res, err)
	}
	assertIndexedCleanupPaths(t, []string{linked, pi, claude}, unknown)
}

func assertIndexedCleanupPaths(t *testing.T, removed []string, retained string) {
	t.Helper()
	for _, path := range removed {
		if exists(path) {
			t.Fatalf("cleanup retained %s", path)
		}
	}
	if !exists(retained) {
		t.Fatal("cleanup removed unverified session")
	}
}

// Cached discovery evidence cannot authorize an idle temp removal.
func TestGCTempFreshClaudeActivityProtectsSharedRemote(t *testing.T) {
	repo, origin, selected, sessions := sharedIdleTemps(t)
	idx := mustIndexSessions(t)
	active, idle := selected[0], selected[1]
	fresh := mkClaudeSession(t, active.Path, "fresh", tempNow)
	plan := planRemotes(selected, nil, false, repo)
	var res gcResult
	warnings := captureStderr(t, func() {
		var err error
		res, err = gcCleanup(repo, selected, plan, false, idx, gcMode{deleteTemp: true, idleOnly: true}, tempNow, nil)
		if err != nil {
			t.Fatal(err)
		}
	})
	assertSharedActiveSurvives(t, repo, origin, active, []string{sessions[0], fresh}, warnings)
	assertSharedIdleRemoved(t, repo, idle, sessions[1], res)
}
