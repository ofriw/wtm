package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discoveryEvidenceFixture(t *testing.T) (string, worktree, string) {
	t.Helper()
	repo, temp, _ := idleTempCheckout(t)
	permanent := canonical(filepath.Join(filepath.Dir(repo), "permanent"))
	gitWorktreeAdd(t, repo, permanent, "permanent", "HEAD")
	commitIdleBaseline(t, permanent)
	backdateGitActivity(t, permanent, tempTestTime)
	w := discovered(t, repo, permanent)
	if !w.unused(time.Now(), time.Hour) {
		t.Fatal("permanent fixture is not idle")
	}
	return repo, temp, permanent
}

func discoveryUnknownSession(t *testing.T, a agent, kind string, age time.Time) string {
	t.Helper()
	p := safetySession(t, a, kind, "{broken}\n", age)
	if kind == "unreadable" {
		makeUnreadable(t, p)
	}
	if kind == "unknown-age" {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, filepath.Join(t.TempDir(), "missing"), p)
	}
	return p
}

func idleGCEvidenceWorkspace(t *testing.T, repo string) (workspace, string) {
	t.Helper()
	var ws workspace
	var err error
	warning := captureStderr(t, func() { ws, err = loadGCWorkspace(&globals{root: repo}, nil) })
	if err != nil {
		t.Fatalf("unattributable file aborted idle-GC discovery: %v", err)
	}
	return ws, warning
}

func requireEvidenceCandidates(t *testing.T, ws workspace, temp worktree, permanent, evidence, warning string) {
	t.Helper()
	if len(ws.idx[temp.Path]) == 0 {
		t.Fatal("idle-GC discovery lost the reusable ownership index")
	}
	candidates := unusedCandidates(ws.wts, time.Now(), ws.ttl)
	if len(candidates) != 1 || candidates[0].Path != temp.Path {
		t.Fatalf("only temp must reach fresh idle check: %+v", candidates)
	}
	if !strings.Contains(warning, evidence) || !strings.Contains(warning, "keeping permanent worktree ACTIVE") {
		t.Fatalf("missing evidence/retention warning: %s", warning)
	}
	if !exists(permanent) {
		t.Fatal("discovery removed permanent checkout")
	}
}

func runDiscoveryEvidenceGC(t *testing.T, repo string, temp worktree, permanent, evidence string, remove bool) {
	t.Helper()
	_, warning, rc := runWTM(t, repo, nil, "gc", "--all", "--yes", "--keep-remote", "--keep-sessions")
	if rc != 0 {
		t.Fatalf("gc failed before candidate-level handling: %s", warning)
	}
	_, evidenceErr := os.Lstat(evidence)
	if exists(temp.Path) == remove || !exists(permanent) || evidenceErr != nil {
		t.Fatalf("gc violated candidate safety: temp=%v permanent=%v evidence=%v", exists(temp.Path), exists(permanent), evidenceErr)
	}
	if remove && !strings.Contains(warning, "predates known activity") {
		t.Fatalf("older evidence warning missing: %s", warning)
	}
	if !remove && !strings.Contains(warning, "skipping") {
		t.Fatalf("fresh evidence refusal missing: %s", warning)
	}
}

func TestIdleGCDiscoveryOlderFileEvidenceReachesFreshTempCheck(t *testing.T) {
	for _, a := range agents() {
		for _, kind := range []string{"unknown", "unreadable"} {
			t.Run(a.name+"/"+kind, func(t *testing.T) {
				repo, temp, permanent := discoveryEvidenceFixture(t)
				p := discoveryUnknownSession(t, a, kind, tempTestTime.Add(-time.Hour))
				ws, warning := idleGCEvidenceWorkspace(t, repo)
				requireEvidenceCandidates(t, ws, temp, permanent, p, warning)
				runDiscoveryEvidenceGC(t, repo, temp, permanent, p, true)
			})
		}
	}
}

func TestIdleGCDiscoveryNewOrUnknownAgeEvidenceBlocksFreshTempCheck(t *testing.T) {
	for _, a := range agents() {
		for _, kind := range []string{"unknown", "unknown-age"} {
			t.Run(a.name+"/"+kind, func(t *testing.T) {
				repo, temp, permanent := discoveryEvidenceFixture(t)
				p := discoveryUnknownSession(t, a, kind, time.Now())
				ws, warning := idleGCEvidenceWorkspace(t, repo)
				requireEvidenceCandidates(t, ws, temp, permanent, p, warning)
				runDiscoveryEvidenceGC(t, repo, temp, permanent, p, false)
			})
		}
	}
}

func TestExplicitGCPathFailsClosedOnUnreadableSession(t *testing.T) {
	for _, a := range agents() {
		t.Run(a.name, func(t *testing.T) {
			repo, temp, permanent := discoveryEvidenceFixture(t)
			p := discoveryUnknownSession(t, a, "unreadable", tempTestTime.Add(-time.Hour))
			_, warning, rc := runWTM(t, repo, nil, "gc", "--path", temp.Path, "--yes", "--keep-remote")
			if rc == 0 || !strings.Contains(warning, p) {
				t.Fatalf("explicit GC must abort and name unreadable session: rc=%d, %s", rc, warning)
			}
			if !exists(temp.Path) || !exists(permanent) || !exists(p) {
				t.Fatal("explicit GC changed checkout or evidence despite unreadable session")
			}
			if _, _, err := discover(repo, nil, true); err == nil || !strings.Contains(err.Error(), p) {
				t.Fatalf("ordinary strict discovery must reject unreadable session: %v", err)
			}
		})
	}
}

func discoveryStructuralFailure(t *testing.T, a agent, kind string) string {
	t.Helper()
	root := filepath.Join(a.configDir(), a.sessionSubdir)
	if kind == "root-file" {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "not a directory", 0o600)
		return root
	}
	p := safetySession(t, a, "inside", "{}\n", tempTestTime)
	dir := filepath.Dir(p)
	if kind == "root-unreadable" {
		dir = root
	}
	makeUnreadableDirectory(t, dir)
	return dir
}

func makeUnreadableDirectory(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("host permits reading mode-000 directories")
	}
}

func TestDisplayDiscoveryKeepsLenientSessionWarning(t *testing.T) {
	for _, a := range agents() {
		t.Run(a.name, func(t *testing.T) {
			repo, _, permanent := discoveryEvidenceFixture(t)
			p := discoveryUnknownSession(t, a, "unknown", tempTestTime)
			var w worktree
			warning := captureStderr(t, func() { w = discovered(t, repo, permanent) })
			if !strings.Contains(warning, p) || !w.unused(time.Now(), time.Hour) {
				t.Fatalf("display must warn without destructive-discovery activity guard: %s, %+v", warning, w)
			}
		})
	}
}

func TestIdleGCDiscoveryStructuralSessionErrorsRemainFatal(t *testing.T) {
	for _, a := range agents() {
		for _, kind := range []string{"root-file", "root-unreadable", "child-unreadable"} {
			t.Run(a.name+"/"+kind, func(t *testing.T) {
				repo, temp, permanent := discoveryEvidenceFixture(t)
				bad := discoveryStructuralFailure(t, a, kind)
				_, err := loadGCWorkspace(&globals{root: repo}, nil)
				if err == nil || !strings.Contains(err.Error(), bad) {
					t.Fatalf("structural failure did not abort discovery: %v", err)
				}
				if !exists(temp.Path) || !exists(permanent) {
					t.Fatal("failed discovery removed checkout")
				}
			})
		}
	}
}
