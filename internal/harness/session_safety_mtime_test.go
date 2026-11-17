package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"wtm/internal/core"
	. "wtm/internal/testutil"
)

func requireSessionTime(t *testing.T, owner string, idx core.SessionIndex, want time.Time) {
	t.Helper()
	for _, strict := range []bool{false, true} {
		got, err := core.SessionLastUsed(owner, idx, strict)
		if err != nil || !got.Equal(want) {
			t.Fatalf("last used (strict=%v) = %s, %v; want %s", strict, got, err, want)
		}
	}
	if got := core.LastUsed(owner, idx); !got.Equal(want) {
		t.Fatalf("display last used = %s, want %s", got, want)
	}
}

func TestSharedSessionSafetyLastUsedContract(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		owner := t.TempDir()
		requireSessionTime(t, owner, core.SessionIndex{}, time.Time{})
		newest := time.Unix(2_000_000, 0)
		content := fmt.Sprintf("{\"cwd\":%q}\n", owner)
		safetySession(t, a, "old", content, newest.Add(-time.Hour))
		safetySession(t, a, "new", content, newest)
		idx, _, err := IndexSessionsStrict(nil)
		if err != nil {
			t.Fatal(err)
		}
		requireSessionTime(t, owner, idx, newest)
	})
}

func TestSharedSessionSafetyReplacedIndexedFile(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		owner := t.TempDir()
		p := safetySession(t, a, "owned", fmt.Sprintf("{\"cwd\":%q}\n", owner), time.Now())
		idx := mustIndexSessions(t)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "foreign.jsonl")
		WriteFile(t, target, "{}\n", 0o600)
		MustSymlink(t, target, p)
		if _, err := core.SessionLastUsed(owner, idx, true); err == nil {
			t.Fatal("indexed transcript replaced by a symlink must stop removal checks")
		}
	})
}

func TestSharedSessionSafetySymlinkedRoot(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		root := filepath.Join(a.ConfigDir(), a.SessionSubdir())
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		MustSymlink(t, t.TempDir(), root)
		owner := t.TempDir()
		p := safetySession(t, a, "owned", fmt.Sprintf("{\"cwd\":%q}\n", owner), time.Now())
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		idx, evidence, err := IndexSessionsStrict(nil)
		if err != nil || len(evidence) != 0 || !idxContains(idx, owner, resolved) {
			t.Fatalf("symlinked root must retain verified ownership: %v, %v, %v", idx, evidence, err)
		}
	})
}
