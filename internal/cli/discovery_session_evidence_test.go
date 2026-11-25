package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/harness"
	. "wtm/internal/testutil"
)

const claudeAgent = "claude"

// agentDirs maps a harness name to its config dir and session subdir. The
// subdir literals mirror the harness agents' private sessionSubdir fields
// (agents.go); the harness intentionally does not export them.
func agentDirs(harnessName string) (string, string) {
	if harnessName == claudeAgent {
		return harness.ClaudeAgent.ConfigDir(), harness.ClaudeSessionSubdir
	}
	return harness.PiAgent.ConfigDir(), harness.PiSessionSubdir
}

// safetySessionFor writes one session file under the harness's session root,
// pins its mtime, and returns the path. The "safety" subdir keeps evidence
// files distinct from real sessions while still being scanned by discovery.
func safetySessionFor(t *testing.T, harnessName, name, content string, age time.Time) string {
	t.Helper()
	cfg, sub := agentDirs(harnessName)
	p := filepath.Join(cfg, sub, "safety", name+".jsonl")
	WriteFile(t, p, content, 0o600)
	SetMtime(t, p, age)
	return p
}

func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if f, err := os.Open(path); err == nil {
		_ = f.Close()
		t.Skip("host allows reading mode-000 files")
	}
}

func discoveryEvidenceFixture(t *testing.T) (string, core.Worktree, string) {
	t.Helper()
	repo, temp, _ := idleTempCheckout(t)
	permanent := CanonicalPath(filepath.Join(filepath.Dir(repo), "permanent"))
	GitWorktreeAdd(t, repo, permanent, "permanent", "HEAD")
	commitIdleBaseline(t, permanent)
	BackdateGitActivity(t, permanent, TempTestTime)
	w := discovered(t, repo, permanent)
	if !w.Unused(time.Now(), time.Hour) {
		t.Fatal("permanent fixture is not idle")
	}
	return repo, temp, permanent
}

func discoveryUnknownSession(t *testing.T, harnessName, kind string, age time.Time) string {
	t.Helper()
	if kind == "unknown-age" {
		return unknownAgeSession(t, harnessName, kind, age)
	}
	p := safetySessionFor(t, harnessName, kind, "{broken}\n", age)
	if kind == "unreadable" {
		makeUnreadable(t, p)
	}
	return p
}

// unknownAgeSession replaces an existing session file with a dangling symlink,
// so the file has no readable age.
func unknownAgeSession(t *testing.T, harnessName, name string, age time.Time) string {
	t.Helper()
	p := safetySessionFor(t, harnessName, name, "{broken}\n", age)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	MustSymlink(t, filepath.Join(t.TempDir(), "missing"), p)
	return p
}

func idleGCEvidenceWorkspace(t *testing.T, repo string) (core.Workspace, string) {
	t.Helper()
	var ws core.Workspace
	var err error
	warning := CaptureStderr(t, func() { ws, err = loadGCWorkspace(&globals{Root: repo}, nil) })
	if err != nil {
		t.Fatalf("unattributable file aborted idle-GC discovery: %v", err)
	}
	return ws, warning
}

func requireEvidenceCandidates(t *testing.T, ws core.Workspace, temp core.Worktree, permanent, evidence, warning string) {
	t.Helper()
	if len(ws.Idx[temp.Path]) == 0 {
		t.Fatal("idle-GC discovery lost the reusable ownership index")
	}
	candidates := unusedCandidates(ws.Wts, time.Now(), ws.TTL)
	if len(candidates) != 1 || candidates[0].Path != temp.Path {
		t.Fatalf("only temp must reach fresh idle check: %+v", candidates)
	}
	if !strings.Contains(warning, evidence) {
		t.Fatalf("missing evidence warning: %s", warning)
	}
	if !fs.Exists(permanent) {
		t.Fatal("discovery removed permanent checkout")
	}
}

func runDiscoveryEvidenceGC(t *testing.T, repo string, temp core.Worktree, permanent, evidence string, remove bool) {
	t.Helper()
	_, warning, rc := RunWTM(t, repo, nil, "gc", "--all", "--yes", "--keep-remote", "--keep-sessions")
	if rc != 0 {
		t.Fatalf("gc failed before candidate-level handling: %s", warning)
	}
	_, evidenceErr := os.Lstat(evidence)
	if fs.Exists(temp.Path) == remove || !fs.Exists(permanent) || evidenceErr != nil {
		t.Fatalf("gc violated candidate safety: temp=%v permanent=%v evidence=%v", fs.Exists(temp.Path), fs.Exists(permanent), evidenceErr)
	}
	if remove && !strings.Contains(warning, "predates known activity") {
		t.Fatalf("older evidence warning missing: %s", warning)
	}
	if !remove && !strings.Contains(warning, "skipping") {
		t.Fatalf("fresh evidence refusal missing: %s", warning)
	}
}

func stageFreshPermanentActivity(t *testing.T, path string) time.Time {
	t.Helper()
	BackdateGitActivity(t, path, TempTestTime.Add(-2*time.Hour))
	file := stageWorktreeActivity(t, core.Worktree{Path: path}, "new")
	SetMtime(t, file, TempTestTime)
	// A real staged change makes the index relevant; a pinned mtime avoids
	// depending on whether a no-change Git command rewrites that file.
	recent := time.Now().Add(-time.Minute).Truncate(time.Second)
	BackdateActivityIndex(t, path, recent)
	return recent
}

func TestIdleGCDiscoveryOlderFileEvidenceReachesFreshTempCheck(t *testing.T) {
	for _, a := range []string{"pi", "claude"} {
		for _, kind := range []string{"unknown", "unreadable"} {
			t.Run(a+"/"+kind, func(t *testing.T) {
				repo, temp, permanent := discoveryEvidenceFixture(t)
				recent := stageFreshPermanentActivity(t, permanent)
				p := discoveryUnknownSession(t, a, kind, TempTestTime.Add(-time.Hour))
				ws, warning := idleGCEvidenceWorkspace(t, repo)
				w := mustFindWorktree(t, ws, permanent)
				if !w.LastUsed.Equal(recent) || w.Unused(time.Now(), ws.TTL) {
					t.Fatalf("staged permanent must be fresh via its index: %+v, want %s", w, recent)
				}
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
	for _, a := range []string{"pi", "claude"} {
		for _, kind := range []string{"unknown", "unknown-age"} {
			t.Run(a+"/"+kind, func(t *testing.T) {
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
	for _, a := range []string{"pi", "claude"} {
		t.Run(a, func(t *testing.T) {
			repo, temp, permanent := discoveryEvidenceFixture(t)
			p := discoveryUnknownSession(t, a, "unreadable", TempTestTime.Add(-time.Hour))
			_, warning, rc := RunWTM(t, repo, nil, "gc", "--path", temp.Path, "--yes", "--keep-remote")
			if rc == 0 || !strings.Contains(warning, p) {
				t.Fatalf("explicit GC must abort and name unreadable session: rc=%d, %s", rc, warning)
			}
			if !fs.Exists(temp.Path) || !fs.Exists(permanent) || !fs.Exists(p) {
				t.Fatal("explicit GC changed checkout or evidence despite unreadable session")
			}
			if _, _, err := harness.DiscoverySessions(true)(nil); err == nil || !strings.Contains(err.Error(), p) {
				t.Fatalf("ordinary strict discovery must reject unreadable session: %v", err)
			}
		})
	}
}

func discoveryStructuralFailure(t *testing.T, harnessName, kind string) string {
	t.Helper()
	cfg, sub := agentDirs(harnessName)
	root := filepath.Join(cfg, sub)
	if kind == "root-file" {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		WriteFile(t, root, "not a directory", 0o600)
		return root
	}
	p := safetySessionFor(t, harnessName, "inside", "{}\n", TempTestTime)
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
	for _, a := range []string{"pi", "claude"} {
		t.Run(a, func(t *testing.T) {
			repo, _, permanent := discoveryEvidenceFixture(t)
			p := discoveryUnknownSession(t, a, "unknown", TempTestTime)
			var w core.Worktree
			warning := CaptureStderr(t, func() { w = discovered(t, repo, permanent) })
			if !strings.Contains(warning, p) || !w.Unused(time.Now(), time.Hour) {
				t.Fatalf("display must warn without destructive-discovery activity guard: %s, %+v", warning, w)
			}
		})
	}
}

func TestIdleGCDiscoveryStructuralSessionErrorsRemainFatal(t *testing.T) {
	for _, a := range discoveryAgents {
		for _, kind := range []string{"root-file", "root-unreadable", "child-unreadable"} {
			t.Run(a+"/"+kind, func(t *testing.T) {
				repo, temp, permanent := discoveryEvidenceFixture(t)
				bad := discoveryStructuralFailure(t, a, kind)
				_, err := loadGCWorkspace(&globals{Root: repo}, nil)
				if err == nil || !strings.Contains(err.Error(), bad) {
					t.Fatalf("structural failure did not abort discovery: %v", err)
				}
				if !fs.Exists(temp.Path) || !fs.Exists(permanent) {
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
	fresh := CanonicalPath(filepath.Join(filepath.Dir(repo), "fresh"))
	GitWorktreeAdd(t, repo, fresh, "fresh", "HEAD")
	BackdateGitActivity(t, fresh, TempNow.Add(-time.Minute))
	discoveryUnknownSession(t, claudeAgent, "unknown", TempTestTime.Add(30*time.Minute))
	ws, _ := idleGCEvidenceWorkspace(t, repo)
	var stale, active core.Worktree
	for _, w := range ws.Wts {
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
	candidates := unusedCandidates(ws.Wts, TempNow, ws.TTL)
	if len(candidates) != 1 || candidates[0].Path != tempWt.Path {
		t.Fatalf("only temp must be candidate after scoped protection: %+v", candidates)
	}
	if !fs.Exists(fresh) || !fs.Exists(permanent) {
		t.Fatal("scoped protection removed checkout")
	}
	// An unknown-age session preserves the original fail-closed behavior:
	// all permanent worktrees stay active.
	unknownAgeSession(t, claudeAgent, "unknown", TempTestTime.Add(30*time.Minute))
	ws, _ = idleGCEvidenceWorkspace(t, repo)
	for _, w := range ws.Wts {
		if !w.Temp && !w.LastUsed.After(now.Add(-time.Minute)) {
			t.Fatalf("unknown-age session must protect all permanent: %+v", w.LastUsed)
		}
	}
}

// mustFindWorktree returns the discovered worktree for path. Guard tests pin
// per-worktree LastUsed, so a missing entry must fail loudly.
func mustFindWorktree(t *testing.T, ws core.Workspace, path string) core.Worktree {
	t.Helper()
	for _, w := range ws.Wts {
		if w.Path == path {
			return w
		}
	}
	t.Fatalf("worktree not discovered: %s", path)
	return core.Worktree{}
}

// requireProtectedNow pins guardPermanentActivity's ACTIVE bump: LastUsed
// lands inside a ±1min window around wall-clock now.
func requireProtectedNow(t *testing.T, w core.Worktree) {
	t.Helper()
	now := time.Now()
	if !w.LastUsed.After(now.Add(-time.Minute)) || w.LastUsed.After(now.Add(time.Minute)) {
		t.Fatalf("permanent not protected to now: %s %+v", w.Path, w.LastUsed)
	}
}

// requireNotProtected pins that the guard left LastUsed at its backdated time,
// far below wall-clock now.
func requireNotProtected(t *testing.T, w core.Worktree) {
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
	edge := CanonicalPath(filepath.Join(filepath.Dir(repo), "edge"))
	GitWorktreeAdd(t, repo, edge, "edge", "HEAD")
	commitIdleBaseline(t, edge)
	BackdateGitActivity(t, edge, TempTestTime.Add(30*time.Minute))
	later := CanonicalPath(filepath.Join(filepath.Dir(repo), "later"))
	GitWorktreeAdd(t, repo, later, "later", "HEAD")
	commitIdleBaseline(t, later)
	BackdateGitActivity(t, later, TempTestTime.Add(45*time.Minute))
	// The session age equals edge's activity exactly: protect-on-== must bump.
	discoveryUnknownSession(t, claudeAgent, "unknown", TempTestTime.Add(30*time.Minute))
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
	mid := CanonicalPath(filepath.Join(filepath.Dir(repo), "mid"))
	GitWorktreeAdd(t, repo, mid, "mid", "HEAD")
	commitIdleBaseline(t, mid)
	BackdateGitActivity(t, mid, TempTestTime.Add(15*time.Minute))
	recent := CanonicalPath(filepath.Join(filepath.Dir(repo), "recent"))
	GitWorktreeAdd(t, repo, recent, "recent", "HEAD")
	commitIdleBaseline(t, recent)
	BackdateGitActivity(t, recent, TempNow.Add(-time.Minute))
	// WHY: safetySession names must differ — discoveryUnknownSession reuses
	// one file path per kind, so two calls with "unknown" would overwrite.
	safetySessionFor(t, "claude", "old", "{broken}\n", TempTestTime.Add(-time.Hour))
	safetySessionFor(t, "claude", "new", "{broken}\n", TempTestTime.Add(30*time.Minute))
	ws, _ := idleGCEvidenceWorkspace(t, repo)
	// WHY: mid sits between the two sessions — max() protects it (bump to
	// now); a min() regression would leave it backdated and unguarded.
	requireProtectedNow(t, mustFindWorktree(t, ws, mid))
	requireNotProtected(t, mustFindWorktree(t, ws, recent))
	candidates := unusedCandidates(ws.Wts, TempNow, ws.TTL)
	if len(candidates) != 1 || candidates[0].Path != tempWt.Path {
		t.Fatalf("only temp must be candidate after max-session protection: %+v", candidates)
	}
}
