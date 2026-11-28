package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	. "wtm/internal/testutil"
	"wtm/internal/ui"
)

func purgeSafetyFixture(t *testing.T, a agent) (string, string, core.SessionIndex) {
	t.Helper()
	owner := t.TempDir()
	p := safetySession(t, a, "owned", fmt.Sprintf("{\"cwd\":%q}\n", owner), time.Now())
	return owner, p, mustIndexSessions(t)
}

func requirePurgeRefused(t *testing.T, owner string, idx core.SessionIndex, keep string) {
	t.Helper()
	before, err := os.ReadFile(keep)
	if err != nil {
		t.Fatal(err)
	}
	n, err := PurgeSessions(owner, idx)
	if err == nil || n != 0 {
		t.Fatalf("unsafe purge: removed=%d error=%v", n, err)
	}
	after, readErr := os.ReadFile(keep)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("unrelated transcript changed: %s: %v", keep, readErr)
	}
}

func TestPurgeRevalidatesCurrentOwner(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		for _, replacement := range []string{fmt.Sprintf("{\"cwd\":%q}\n", t.TempDir()), "{}\n", "{broken}\n", "{\"cwd\":\"relative\"}\n"} {
			owner, p, idx := purgeSafetyFixture(t, a)
			WriteFile(t, p, replacement, 0o600)
			requirePurgeRefused(t, owner, idx, p)
		}
	})
}

func TestPurgeRejectsReplacedFileLink(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		owner, p, idx := purgeSafetyFixture(t, a)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(t.TempDir(), "unrelated.jsonl")
		WriteFile(t, keep, fmt.Sprintf("{\"cwd\":%q}\n", owner), 0o600)
		MustSymlink(t, keep, p)
		requirePurgeRefused(t, owner, idx, keep)
	})
}

func TestPurgeUsesHarnessOwnerParser(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		owner, p, idx := purgeSafetyFixture(t, a)
		WriteFile(t, p, cwdAfterRecords(owner, 1), 0o600)
		if a.name == PiAgent.name {
			requirePurgeRefused(t, owner, idx, p)
		} else {
			assertOwnedSessionPurged(t, owner, idx, p)
		}
	})
}

func redirectSessionBoundary(t *testing.T, owner, p, boundary string) string {
	t.Helper()
	if err := os.Rename(boundary, boundary+"-original"); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(boundary, p)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	keep := filepath.Join(target, rel)
	// Same-owner data cannot authorize deletion through a replaced boundary.
	WriteFile(t, keep, fmt.Sprintf("{\"cwd\":%q}\n", owner), 0o600)
	MustSymlink(t, target, boundary)
	return keep
}

func TestPurgeRejectsRedirectedAncestors(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		for depth := 0; depth < 4; depth++ {
			t.Run(fmt.Sprint(depth), func(t *testing.T) {
				Sandbox(t)
				owner := t.TempDir()
				p := safetySession(t, a, "nested/deep/owned", fmt.Sprintf("{\"cwd\":%q}\n", owner), time.Now())
				idx := mustIndexSessions(t)
				boundary := filepath.Dir(p)
				for n := 0; n < depth; n++ {
					boundary = filepath.Dir(boundary)
				}
				keep := redirectSessionBoundary(t, owner, p, boundary)
				requirePurgeRefused(t, owner, idx, keep)
			})
		}
	})
}

func TestPurgeSupportsIndexedRootSymlink(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		root := filepath.Join(a.ConfigDir(), a.sessionSubdir)
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		MustSymlink(t, t.TempDir(), root)
		owner, p, idx := purgeSafetyFixture(t, a)
		assertOwnedSessionPurged(t, owner, idx, p)
		if _, err := os.Lstat(root); err != nil {
			t.Fatalf("purge removed root link: %v", err)
		}
	})
}

func requireRootPolicy(t *testing.T, root string) {
	t.Helper()
	if _, _, err := IndexSessionsStrict(nil); err == nil || !strings.Contains(err.Error(), root) {
		t.Fatalf("strict scan accepted unreadable Root: %v", err)
	}
	warning := CaptureStderr(t, func() {
		if _, err := indexSessions(agents(), core.SessionErrPolicy{}); err != nil {
			t.Fatalf("display scan failed: %v", err)
		}
	})
	if !strings.Contains(warning, root) || !strings.Contains(warning, ui.WarnPrefix) {
		t.Fatalf("display omitted root warning: %q", warning)
	}
}

func TestSessionRootFailurePolicy(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		root := filepath.Join(a.ConfigDir(), a.sessionSubdir)
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		WriteFile(t, root, "not a directory", 0o600)
		requireRootPolicy(t, root)
	})
}

func TestSessionUnreadableRootPolicy(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		_, p, _ := purgeSafetyFixture(t, a)
		root := filepath.Dir(filepath.Dir(p))
		if err := os.Chmod(root, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
		if _, err := os.ReadDir(root); err == nil {
			t.Skip("host permits reading mode-000 directories")
		}
		requireRootPolicy(t, root)
	})
}
