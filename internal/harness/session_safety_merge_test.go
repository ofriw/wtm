package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/temp"
	. "wtm/internal/testutil"
)

func forSessionAgents(t *testing.T, check func(*testing.T, agent)) {
	t.Helper()
	for _, a := range agents() {
		t.Run(a.name, func(t *testing.T) {
			Sandbox(t)
			check(t, a)
		})
	}
}

func safetySession(t *testing.T, a agent, name, content string, age time.Time) string {
	t.Helper()
	p := filepath.Join(a.ConfigDir(), a.sessionSubdir, "safety", name+".jsonl")
	WriteFile(t, p, content, 0o600)
	if err := os.Chtimes(p, age, age); err != nil {
		t.Fatal(err)
	}
	return p
}

func requireSessionEvidence(t *testing.T, p string, age time.Time) {
	t.Helper()
	_, evidence, err := IndexSessionsStrict(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range evidence {
		if u.Path == p && u.ModTime.Equal(age) {
			return
		}
	}
	t.Fatalf("missing activity evidence for %s at %s: %+v", p, age, evidence)
}

func TestSharedSessionSafetyUnknownOwners(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		age := time.Unix(2_000_000, 0)
		for _, content := range []string{"{broken}\n", "{}\n", "{\"cwd\":\"relative\"}\n", ""} {
			p := safetySession(t, a, "unknown", content, age)
			requireSessionEvidence(t, p, age)
			warning := CaptureStderr(t, func() { mustIndexSessions(t) })
			if !strings.Contains(warning, p) {
				t.Fatalf("discovery did not disclose unknown ownership: %q", warning)
			}
		}
	})
}

func TestSharedSessionSafetyFileSymlinkAge(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		age := time.Unix(2_000_000, 0)
		target := filepath.Join(t.TempDir(), "target")
		WriteFile(t, target, "{}\n", 0o600)
		if err := os.Chtimes(target, age, age); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(a.ConfigDir(), a.sessionSubdir, "linked.jsonl")
		MustSymlink(t, target, link)
		requireSessionEvidence(t, link, age)
	})
}

func TestSharedSessionSafetyDirectoryAndDanglingLinks(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		root := filepath.Join(a.ConfigDir(), a.sessionSubdir)
		for _, target := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
			link := filepath.Join(root, fmt.Sprintf("link-%d", len(target)))
			MustSymlink(t, target, link)
			requireSessionEvidence(t, link, time.Time{})
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestSharedSessionSafetyMissingIndexedFile(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		owner := t.TempDir()
		p := safetySession(t, a, "owned", fmt.Sprintf("{\"cwd\":%q}\n", owner), time.Now())
		idx, _, err := IndexSessionsStrict(nil)
		if err != nil || !idxContains(idx, owner, p) {
			t.Fatalf("owned session not indexed: %v, %v", idx, err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if _, err := core.SessionLastUsed(owner, idx, true); err == nil {
			t.Fatal("missing indexed session accepted by removal check")
		}
		warning := CaptureStderr(t, func() { core.LastUsed(owner, idx) })
		if !strings.Contains(warning, p) {
			t.Fatalf("display did not disclose missing activity: %q", warning)
		}
	})
}

func TestSharedSessionSafetyUnreadableChild(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		p := safetySession(t, a, "inside", "{}\n", time.Now())
		dir := filepath.Dir(p)
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if _, err := os.ReadDir(dir); err == nil {
			t.Skip("host permits reading mode-000 directories")
		}
		if _, _, err := IndexSessionsStrict(nil); err == nil || !strings.Contains(err.Error(), dir) {
			t.Fatalf("unreadable child must stop removal checks: %v", err)
		}
		warning := CaptureStderr(t, func() { _, _ = indexSessions(agents(), core.SessionErrPolicy{}) })
		if !strings.Contains(warning, dir) {
			t.Fatalf("display did not disclose unreadable directory: %q", warning)
		}
	})
}

func TestSharedSessionSafetyUnreadableFileEvidence(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		age := time.Unix(2_000_000, 0)
		p := safetySession(t, a, "unreadable", "{}\n", age)
		makeUnreadable(t, p)
		requireSessionEvidence(t, p, age)
	})
}

func TestSharedSessionSafetyRootMustBeDirectory(t *testing.T) {
	forSessionAgents(t, func(t *testing.T, a agent) {
		root := filepath.Join(a.ConfigDir(), a.sessionSubdir)
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		WriteFile(t, root, "not a directory", 0o600)
		if _, _, err := IndexSessionsStrict(nil); err == nil || !strings.Contains(err.Error(), root) {
			t.Fatalf("invalid session root must stop removal checks: %v", err)
		}
	})
}

func TestSharedSessionSafetyEvidenceAge(t *testing.T) {
	known := time.Unix(2_000_000, 0)
	for _, age := range []time.Time{time.Time{}, known.Add(time.Second), known, known.Add(-time.Second)} {
		want := age.IsZero() || age.After(known)
		if got := (temp.UnverifiedSession{ModTime: age}).BlockRemoval(known); got != want {
			t.Fatalf("activity at %s blocks %s = %v, want %v", age, known, got, want)
		}
	}
}
