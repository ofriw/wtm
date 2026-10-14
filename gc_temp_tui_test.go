//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Keep the fixture idle without changing the binary's clock or removal paths.
func gcTempTUIFixture(t *testing.T) (string, worktree, string) {
	t.Helper()
	repo, path, session := gcTUIFixture(t)
	branch := "tmp/gc-consent"
	mustGit(t, path, "branch", "-m", branch)
	seedTempRecord(t, path, branch, "1h", gcTUISeedTime())
	return repo, discovered(t, repo, canonical(path)), session
}

// Consent keys follow the complete branch disclosure, not merely the picker.
func runGCTempConsent(t *testing.T, repo string, w worktree, answer string) (string, string) {
	t.Helper()
	prompt := "Remove 1 worktree(s); force-delete temp LOCAL branches: " + w.Branch + "?"
	stdout, stderr, rc := runGCTTY(t, repo, gcTTYWidth, []gcTUIStep{
		{want: "Select worktrees to remove", send: " \r"},
		{want: prompt, send: answer},
	}, "--keep-remote")
	if rc != 0 || !strings.Contains(stderr, prompt) {
		t.Fatalf("temp consent rc=%d; want disclosed branch %q\nstdout: %s\nstderr: %s", rc, w.Branch, stdout, stderr)
	}
	return stdout, stderr
}

func TestGCTUITempDeclinePreservesResources(t *testing.T) {
	repo, w, session := gcTempTUIFixture(t)
	store := mustReadTempStore(t)
	head := mustGit(t, repo, "rev-parse", w.Branch)
	checkout := readTempTUIFile(t, filepath.Join(w.Path, "README.md"))
	sessionData := readTempTUIFile(t, session)
	stdout, stderr := runGCTempConsent(t, repo, w, "n")
	if strings.Contains(stdout, "removed") || strings.Contains(stdout, "deleted branch") || !strings.Contains(stderr, "aborted") {
		t.Fatalf("decline must abort without deletions; stdout=%q stderr=%q", stdout, stderr)
	}
	assertWorktreeIntact(t, w.Path, session)
	assertWorktreeListed(t, repo, w.Path)
	assertTempStore(t, store)
	assertTempTUIFile(t, filepath.Join(w.Path, "README.md"), checkout)
	assertTempTUIFile(t, session, sessionData)
	if got := mustGit(t, repo, "rev-parse", w.Branch); got != head {
		t.Fatalf("decline changed local branch: got %q, want %q", got, head)
	}
}

func TestGCTUITempAcceptRemovesDisclosedResources(t *testing.T) {
	repo, w, session := gcTempTUIFixture(t)
	store := mustReadTempStore(t)
	delete(store, w.Path)
	stdout, _ := runGCTempConsent(t, repo, w, "y")
	for _, want := range []string{"removed " + w.Path, "deleted branch " + w.Branch} {
		if !strings.Contains(stdout, want) {
			t.Errorf("accept report missing %q: %s", want, stdout)
		}
	}
	for _, path := range []string{w.Path, session} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("accept must remove %s: stat error %v", path, err)
		}
	}
	assertWorktreeNotListed(t, repo, w.Path)
	assertTempStore(t, store)
	assertTempTUIBranchGone(t, repo, w.Branch)
}

func assertTempTUIBranchGone(t *testing.T, repo, branch string) {
	t.Helper()
	if got := mustGit(t, repo, "branch", "--list", branch); strings.TrimSpace(got) != "" {
		t.Errorf("accept retained disclosed local branch %s: %s", branch, got)
	}
}

func readTempTUIFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertTempTUIFile(t *testing.T, path, want string) {
	t.Helper()
	if got := readTempTUIFile(t, path); got != want {
		t.Errorf("decline changed %s: got %q, want %q", path, got, want)
	}
}
