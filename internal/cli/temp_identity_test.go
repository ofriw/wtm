package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/temp"
	. "wtm/internal/testutil"
)

// discoveredWorktree composes one inventory entry through the production
// discovery seam, so fixtures exercise the same wiring the real status/gc
// commands use.
func discoveredWorktree(t *testing.T, repo, path string) core.Worktree {
	t.Helper()
	ws, err := core.LoadWorkspace(&core.Globals{Root: repo}, false, defaultHooks())
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	for _, w := range ws.Wts {
		if w.Path == CanonicalPath(path) {
			return w
		}
	}
	t.Fatalf("worktree %s not discovered", path)
	return core.Worktree{}
}

func identityCheckout(t *testing.T) (string, string, core.Worktree) {
	t.Helper()
	Sandbox(t)
	repo := InitRepo(t, "main")
	MustGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	path := filepath.Join(filepath.Dir(repo), "temp")
	GitWorktreeAdd(t, repo, path, "tmp/test", "HEAD")
	seedTempRecord(t, path, "tmp/test", "100ms", TempTestTime)
	w := discoveredWorktree(t, repo, path)
	if !w.Temp || w.TempIdentity == "" {
		t.Fatalf("identity was not attached: %+v", w)
	}
	return repo, path, w
}

func assertPermanentAfterReconcile(t *testing.T, repo, path string) {
	t.Helper()
	before := discoveredWorktree(t, repo, path)
	if before.Temp {
		t.Fatal("unverified checkout inherited temp rules before reconciliation")
	}
	if err := temp.ReconcileTempStore(); err != nil {
		t.Fatal(err)
	}
	store := mustReadTempStore(t)
	if _, ok := store[CanonicalPath(path)]; ok {
		t.Fatal("stale record survived reconciliation")
	}
	w := discoveredWorktree(t, repo, path)
	if w.Temp {
		t.Fatal("replacement checkout became temp")
	}
}

func TestTempIdentitySameBranchRecreation(t *testing.T) {
	repo, path, old := identityCheckout(t)
	MustGit(t, repo, "worktree", "remove", path)
	MustGit(t, repo, "worktree", "add", path, "tmp/test")
	if err := verifyTempWorktree(old); err == nil {
		t.Fatal("same branch recreation passed removal verification")
	}
	assertPermanentAfterReconcile(t, repo, path)
}

func TestTempIdentityPathReuse(t *testing.T) {
	repo, path, old := identityCheckout(t)
	MustGit(t, repo, "worktree", "remove", path)
	GitWorktreeAdd(t, repo, path, "permanent", "HEAD")
	if err := verifyTempWorktree(old); err == nil {
		t.Fatal("path reuse passed removal verification")
	}
	assertPermanentAfterReconcile(t, repo, path)
}

func TestTempIdentitySwitchedBranch(t *testing.T) {
	repo, path, old := identityCheckout(t)
	MustGit(t, path, "switch", "-c", "permanent")
	if err := verifyTempWorktree(old); err == nil {
		t.Fatal("branch switch passed removal verification")
	}
	assertPermanentAfterReconcile(t, repo, path)
}

func TestTempIdentityChangedBeforeRemoval(t *testing.T) {
	_, path, old := identityCheckout(t)
	if _, err := temp.AddTempRecord(path, temp.Record{CreatedAt: TempNow, TTL: "1h", Branch: old.Branch}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTempWorktree(old); err == nil {
		t.Fatal("changed registry identity passed verification")
	}
	rec := mustReadTempStore(t)[CanonicalPath(path)]
	if rec.Identity == old.TempIdentity {
		t.Fatal("new record reused identity")
	}
}

func TestTempIdentityAdminTokenChanged(t *testing.T) {
	repo, path, old := identityCheckout(t)
	tokenPath, err := temp.TempIdentityPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(tokenPath, path+string(os.PathSeparator)) {
		t.Fatal("identity stored inside checkout")
	}
	if err := os.WriteFile(tokenPath, []byte(fixtureTempIdentity), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyTempWorktree(old); err == nil {
		t.Fatal("changed admin token passed verification")
	}
	assertPermanentAfterReconcile(t, repo, path)
}

func TestPromotionStrictMalformedRecord(t *testing.T) {
	_, path, _ := identityCheckout(t)
	registry, _ := fs.WtmFilePath("temp.json")
	cases := []string{"{", "null", `{"target":null}`, `{"target":{"ttl":"bogus"}}`}
	for _, data := range cases {
		if err := os.WriteFile(registry, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := temp.PromoteTempRecord(path); err == nil {
			t.Fatalf("malformed registry accepted: %s", data)
		}
	}
}

func TestPromotionUnreadableRegistry(t *testing.T) {
	_, path, _ := identityCheckout(t)
	registry, _ := fs.WtmFilePath("temp.json")
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(registry, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := temp.PromoteTempRecord(path); err == nil {
		t.Fatal("unreadable registry reported permanent")
	}
}

func TestPromotionJSONAndIdentityRemoval(t *testing.T) {
	repo, path, w := identityCheckout(t)
	out := CaptureStdout(t, func() {
		if err := promoteWorktree(&core.Globals{Root: repo, JSON: true}, w.Branch); err != nil {
			t.Fatal(err)
		}
	})
	var result promoteResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Promoted || result.Path != CanonicalPath(path) || result.Branch != w.Branch {
		t.Fatalf("promotion JSON: %+v", result)
	}
	assertPromotionState(t, path, w.Branch)
}

// A temp worktree is addressed by its bare name: `promote review` resolves to
// tmp/review, the same way `delete review` does.
func TestPromoteMatchesTempPrefix(t *testing.T) {
	repo, path, w := identityCheckout(t)
	out := CaptureStdout(t, func() {
		if err := promoteWorktree(&core.Globals{Root: repo, JSON: true}, "test"); err != nil {
			t.Fatal(err)
		}
	})
	var result promoteResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Promoted || result.Branch != w.Branch {
		t.Fatalf("promotion JSON: %+v", result)
	}
	assertPromotionState(t, path, w.Branch)
}

func assertPromotionState(t *testing.T, path, branch string) {
	t.Helper()
	tokenPath, _ := temp.TempIdentityPath(path)
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("identity remains: %v", err)
	}
	if promoted, err := temp.PromoteTempRecord(CanonicalPath(path)); err != nil || promoted {
		t.Fatalf("retry = %v, %v", promoted, err)
	}
	if got := MustGit(t, path, "branch", "--show-current"); strings.TrimSpace(got) != branch {
		t.Fatal("promotion changed branch")
	}
}

func restrictRegistryWrites(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permission contract")
	}
	registry, _ := fs.WtmFilePath("temp.json")
	dir := filepath.Dir(registry)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	probe := filepath.Join(dir, "permission-probe")
	if err := os.WriteFile(probe, nil, 0600); err == nil {
		_ = os.Remove(probe)
		// Six fail-closed tests rest on this. A privileged run proves nothing
		// and hides that silently, so refuse to lose the contract where it is
		// the only coverage: CI must not run these as root.
		if os.Getenv("CI") != "" {
			t.Fatal("process bypasses permissions: fail-closed contract untested")
		}
		t.Skip("process bypasses permissions")
	}
}

func TestPromotionCommandRejectsCorruptTargetRecord(t *testing.T) {
	repo, path, w := identityCheckout(t)
	registry, _ := fs.WtmFilePath("temp.json")
	data, err := json.Marshal(map[string]any{CanonicalPath(path): map[string]any{"ttl": "broken"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registry, data, 0600); err != nil {
		t.Fatal(err)
	}
	CaptureStderr(t, func() {
		if err := promoteWorktree(&core.Globals{Root: repo, JSON: true}, w.Branch); err == nil {
			t.Fatal("corrupt target record was silently reported permanent")
		}
	})
}

func TestClearTempRecordWriteFailure(t *testing.T) {
	_, path, w := identityCheckout(t)
	restrictRegistryWrites(t)
	if err := clearTempRecord(CanonicalPath(path)); err == nil {
		t.Fatal("clear write failure reported success")
	}
	if err := verifyTempWorktree(w); err != nil {
		t.Fatalf("failed clear changed temp identity: %v", err)
	}
}

func TestTempRecordWriteFailure(t *testing.T) {
	_, path, old := identityCheckout(t)
	restrictRegistryWrites(t)
	_, err := temp.AddTempRecord(path, temp.Record{CreatedAt: TempNow, TTL: "1h", Branch: old.Branch})
	if err == nil {
		t.Fatal("record write failure reported success")
	}
	if rec := mustReadTempStore(t)[CanonicalPath(path)]; rec.Identity != old.TempIdentity {
		t.Fatal("failed mutation replaced registry identity")
	}
	if err := verifyTempWorktree(old); err == nil {
		t.Fatal("failed mutation left stale temp permission verified")
	}
}

func TestTempRecordInvalidIdentityRejected(t *testing.T) {
	_, path, old := identityCheckout(t)
	_, err := temp.AddTempRecord(path, temp.Record{CreatedAt: TempNow, TTL: "1h", Branch: old.Branch, Identity: "invalid"})
	if err == nil {
		t.Fatal("invalid supplied identity accepted")
	}
	if err := verifyTempWorktree(old); err != nil {
		t.Fatalf("invalid add changed existing identity: %v", err)
	}
}

func TestPromotionRegistryWriteFailure(t *testing.T) {
	_, path, _ := identityCheckout(t)
	restrictRegistryWrites(t)
	if _, err := temp.PromoteTempRecord(CanonicalPath(path)); err == nil {
		t.Fatal("write failure reported success")
	}
	if _, ok := mustReadTempStore(t)[CanonicalPath(path)]; !ok {
		t.Fatal("failed promotion discarded registry record")
	}
	tokenPath, _ := temp.TempIdentityPath(path)
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatal("failed promotion removed identity")
	}
}
