package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/harness"
	. "wtm/internal/testutil"
	"wtm/internal/ui"
)

func hiddenSessionDirectory(t *testing.T) string {
	t.Helper()
	target := t.TempDir()
	file := filepath.Join(target, "active.jsonl")
	WriteFile(t, file, "{}\n", 0o600)
	if err := os.Chtimes(target, TempTestTime, TempTestTime); err != nil {
		t.Fatal(err)
	}
	// Updating an existing session leaves its containing directory old.
	WriteFile(t, file, "{}\n{}\n", 0o600)
	link := filepath.Join(harness.PiAgent.ConfigDir(), "sessions", "hidden")
	MustSymlink(t, target, link)
	return link
}

func cleanupHiddenSession(t *testing.T, repo string, w core.Worktree, idx core.SessionIndex) (gcResult, []string, error) {
	t.Helper()
	var res gcResult
	events := make(chan ui.ProgressEvent, 64)
	p := ui.NewProgress(context.Background(), events)
	res, err := gcCleanup(repo, []core.Worktree{w}, remotePlan{keep: true}, false, idx, gcMode{deleteTemp: true, idleOnly: true}, TempNow, p)
	close(events)
	var notices []string
	for ev := range events {
		if ev.Kind == ui.EvNotice {
			notices = append(notices, ev.Text)
		}
	}
	return res, notices, err
}

func TestPiSymlinkDirectoryBlocksIdleGC(t *testing.T) {
	repo, w, session := idleTempCheckout(t)
	before := mustReadTempStore(t)
	idx := cleanupSessionIndex(t, repo)
	link := hiddenSessionDirectory(t)
	res, notices, err := cleanupHiddenSession(t, repo, w, idx)
	assertTempStore(t, before)
	if !fs.Exists(w.Path) || !fs.Exists(session) || !git.RefExists(repo, "refs/heads/"+w.Branch) {
		t.Fatal("unknown-age session directory must preserve checkout, sessions and branch")
	}
	// Unknown age fails closed as a skip, not a failure: the checkout is safe.
	if err != nil || len(res.Removed) != 0 || len(res.Skipped) != 1 || res.Skipped[0] != w.Path || len(res.Failed) != 0 {
		t.Fatalf("unknown-age activity must skip idle GC: %+v, %v", res, err)
	}
	if !strings.Contains(strings.Join(notices, "\n"), link) {
		t.Fatalf("skip must identify the hidden session directory: %v", notices)
	}
}

func TestPiSymlinkDirectoryHasUnknownAge(t *testing.T) {
	Sandbox(t)
	link := hiddenSessionDirectory(t)
	info, err := os.Stat(link)
	if err != nil || !info.ModTime().Equal(TempTestTime) {
		t.Fatalf("fixture directory age = %v, %v", info, err)
	}
	_, unverified, err := harness.IndexSessionsStrict(nil)
	if err != nil || len(unverified) != 1 {
		t.Fatalf("symlink discovery = %v, %v", unverified, err)
	}
	if unverified[0].Path != link || !unverified[0].ModTime.IsZero() {
		t.Fatalf("symlink directory must have unknown age: %+v", unverified[0])
	}
}

func TestPiSymlinkFileRetainsTargetAge(t *testing.T) {
	for _, age := range []time.Time{TempTestTime, TempNow} {
		t.Run(age.Format(time.RFC3339), func(t *testing.T) {
			Sandbox(t)
			target := filepath.Join(t.TempDir(), "target.jsonl")
			WriteFile(t, target, "{}\n", 0o600)
			if err := os.Chtimes(target, age, age); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(harness.PiAgent.ConfigDir(), "sessions", "linked.jsonl")
			MustSymlink(t, target, link)
			_, unverified, err := harness.IndexSessionsStrict(nil)
			if err != nil || len(unverified) != 1 || !unverified[0].ModTime.Equal(age) {
				t.Fatalf("file symlink target age = %+v, %v; want %v", unverified, err, age)
			}
		})
	}
}
