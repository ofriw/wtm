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
	if !strings.Contains(warning, evidence) {
		t.Fatalf("missing evidence warning: %s", warning)
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
				// WHY: the amend is real recent git activity — it refreshes the
				// private index mtime to wall-clock now, so the permanent is fresh:
				// the older unverified session neither protects it (scoped guard)
				// nor blocks the temp's fresh idle check. Dates are not scanned here;
				// the index mtime decides.
				backdateGitActivity(t, permanent, tempTestTime.Add(-2*time.Hour))
				mustGit(t, permanent, "commit", "--amend", "--no-edit")
				p := discoveryUnknownSession(t, a, kind, tempTestTime.Add(-time.Hour))
				ws, warning := idleGCEvidenceWorkspace(t, repo)
				requireEvidenceCandidates(t, ws, temp, permanent, p, warning)
				// WHY: the older session must not trigger the blanket permanent
				// bump — a regression to unknown-age behavior belongs to the fresh
				// check below, and would wrongly hide this permanent from GC.
				if strings.Contains(warning, "keeping permanent worktree ACTIVE") {
					t.Fatalf("older evidence blanket-bumped permanents: %s", warning)
				}
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
				// WHY: fresh/unknown-age evidence keeps each permanent ACTIVE;
				// the older-file case below correctly emits no such warning.
				if !strings.Contains(warning, "keeping permanent worktree ACTIVE") {
					t.Fatalf("missing retention warning: %s", warning)
				}
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

// TestIdleGCDiscoveryScopedPermanentProtection pins that guardPermanentActivity
// protects only permanent worktrees older than the newest known-age unverified
// session, not every permanent worktree in the repo.
func TestIdleGCDiscoveryScopedPermanentProtection(t *testing.T) {
	repo, tempWt, permanent := discoveryEvidenceFixture(t)
	fresh := canonical(filepath.Join(filepath.Dir(repo), "fresh"))
	gitWorktreeAdd(t, repo, fresh, "fresh", "HEAD")
	backdateGitActivity(t, fresh, tempNow.Add(-time.Minute))
	p := discoveryUnknownSession(t, claudeAgent, "unknown", tempTestTime.Add(30*time.Minute))
	ws, _ := idleGCEvidenceWorkspace(t, repo)
	var stale, active worktree
	for _, w := range ws.wts {
		if w.Path == permanent {
			stale = w
		}
		if w.Path == fresh {
			active = w
		}
	}
	now := time.Now()
	if !stale.LastUsed.After(now.Add(-time.Minute)) || stale.LastUsed.After(now.Add(time.Minute)) {
		t.Fatalf("stale permanent not protected: %+v", stale.LastUsed)
	}
	// WHY: stale bump lands near wall-clock now (2026) while the fresh
	// checkout stays near its backdated 2024 time. The old blanket guard
	// bumped both, so only a not-after-now check pins the new scoping.
	if active.LastUsed.After(now.Add(-time.Minute)) {
		t.Fatalf("fresh permanent was incorrectly protected: %+v", active.LastUsed)
	}
	candidates := unusedCandidates(ws.wts, tempNow, ws.ttl)
	if len(candidates) != 1 || candidates[0].Path != tempWt.Path {
		t.Fatalf("only temp must be candidate after scoped protection: %+v", candidates)
	}
	if !exists(fresh) || !exists(permanent) {
		t.Fatal("scoped protection removed checkout")
	}
	// An unknown-age session preserves the original fail-closed behavior:
	// all permanent worktrees stay active.
	os.Remove(p)
	mustSymlink(t, filepath.Join(t.TempDir(), "missing"), p)
	ws, _ = idleGCEvidenceWorkspace(t, repo)
	for _, w := range ws.wts {
		if !w.Temp && !w.LastUsed.After(now.Add(-time.Minute)) {
			t.Fatalf("unknown-age session must protect all permanent: %+v", w.LastUsed)
		}
	}
}

// mustFindWorktree returns the discovered worktree for path. Guard tests pin
// per-worktree LastUsed, so a missing entry must fail loudly.
func mustFindWorktree(t *testing.T, ws workspace, path string) worktree {
	t.Helper()
	for _, w := range ws.wts {
		if w.Path == path {
			return w
		}
	}
	t.Fatalf("worktree not discovered: %s", path)
	return worktree{}
}

// requireProtectedNow pins guardPermanentActivity's ACTIVE bump: LastUsed
// lands inside a ±1min window around wall-clock now.
func requireProtectedNow(t *testing.T, w worktree) {
	t.Helper()
	now := time.Now()
	if !w.LastUsed.After(now.Add(-time.Minute)) || w.LastUsed.After(now.Add(time.Minute)) {
		t.Fatalf("permanent not protected to now: %s %+v", w.Path, w.LastUsed)
	}
}

// requireNotProtected pins that the guard left LastUsed at its backdated time,
// far below wall-clock now.
func requireNotProtected(t *testing.T, w worktree) {
	t.Helper()
	if w.LastUsed.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("permanent was incorrectly protected: %s %+v", w.Path, w.LastUsed)
	}
}

// TestIdleGCDiscoveryGuardProtectsOnExactSessionAge pins the equality edge of
// guardPermanentActivity (status.go:245): a permanent whose newest known-age
// unverified session matches its activity exactly must stay protected.
func TestIdleGCDiscoveryGuardProtectsOnExactSessionAge(t *testing.T) {
	repo, _, _ := discoveryEvidenceFixture(t)
	edge := canonical(filepath.Join(filepath.Dir(repo), "edge"))
	gitWorktreeAdd(t, repo, edge, "edge", "HEAD")
	commitIdleBaseline(t, edge)
	backdateGitActivity(t, edge, tempTestTime.Add(30*time.Minute))
	later := canonical(filepath.Join(filepath.Dir(repo), "later"))
	gitWorktreeAdd(t, repo, later, "later", "HEAD")
	commitIdleBaseline(t, later)
	backdateGitActivity(t, later, tempTestTime.Add(45*time.Minute))
	// The session age equals edge's activity exactly: protect-on-== must bump.
	discoveryUnknownSession(t, claudeAgent, "unknown", tempTestTime.Add(30*time.Minute))
	ws, _ := idleGCEvidenceWorkspace(t, repo)
	requireProtectedNow(t, mustFindWorktree(t, ws, edge))
	// WHY: protect-on-equality is the intentional fail-closed asymmetry —
	// permanents are precious, so == newest session keeps them ACTIVE, while
	// the temp path removes only on strict >.
	requireNotProtected(t, mustFindWorktree(t, ws, later))
}

// TestIdleGCDiscoveryNewestSessionDecidesProtection pins newestKnownSession
// (status.go:261): protection scopes by the MAX of multiple known-age
// unverified sessions, not the min or first.
func TestIdleGCDiscoveryNewestSessionDecidesProtection(t *testing.T) {
	repo, tempWt, _ := discoveryEvidenceFixture(t)
	mid := canonical(filepath.Join(filepath.Dir(repo), "mid"))
	gitWorktreeAdd(t, repo, mid, "mid", "HEAD")
	commitIdleBaseline(t, mid)
	backdateGitActivity(t, mid, tempTestTime.Add(15*time.Minute))
	recent := canonical(filepath.Join(filepath.Dir(repo), "recent"))
	gitWorktreeAdd(t, repo, recent, "recent", "HEAD")
	commitIdleBaseline(t, recent)
	backdateGitActivity(t, recent, tempNow.Add(-time.Minute))
	// WHY: safetySession names must differ — discoveryUnknownSession reuses
	// one file path per kind, so two calls with "unknown" would overwrite.
	safetySession(t, claudeAgent, "old", "{broken}\n", tempTestTime.Add(-time.Hour))
	safetySession(t, claudeAgent, "new", "{broken}\n", tempTestTime.Add(30*time.Minute))
	ws, _ := idleGCEvidenceWorkspace(t, repo)
	// WHY: mid sits between the two sessions — max() protects it (bump to
	// now); a min() regression would leave it backdated and unguarded.
	requireProtectedNow(t, mustFindWorktree(t, ws, mid))
	requireNotProtected(t, mustFindWorktree(t, ws, recent))
	candidates := unusedCandidates(ws.wts, tempNow, ws.ttl)
	if len(candidates) != 1 || candidates[0].Path != tempWt.Path {
		t.Fatalf("only temp must be candidate after max-session protection: %+v", candidates)
	}
}
