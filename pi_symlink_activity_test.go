package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hiddenSessionDirectory(t *testing.T) string {
	t.Helper()
	target := t.TempDir()
	file := filepath.Join(target, "active.jsonl")
	writeFile(t, file, "{}\n", 0o600)
	if err := os.Chtimes(target, tempTestTime, tempTestTime); err != nil {
		t.Fatal(err)
	}
	// Updating an existing session leaves its containing directory old.
	writeFile(t, file, "{}\n{}\n", 0o600)
	link := filepath.Join(piAgentDir(), "sessions", "hidden")
	mustSymlink(t, target, link)
	return link
}

func cleanupHiddenSession(t *testing.T, repo string, w worktree) (gcResult, []string, error) {
	t.Helper()
	var res gcResult
	events := make(chan progressEvent, 64)
	p := &progress{ctx: context.Background(), events: events}
	res, err := gcCleanup(repo, []worktree{w}, remotePlan{keep: true}, false, gcMode{deleteTemp: true, idleOnly: true}, tempNow, p)
	close(events)
	var notices []string
	for ev := range events {
		if ev.kind == evNotice {
			notices = append(notices, ev.text)
		}
	}
	return res, notices, err
}

func TestPiSymlinkDirectoryBlocksIdleGC(t *testing.T) {
	repo, w, session := idleTempCheckout(t)
	before := mustReadTempStore(t)
	link := hiddenSessionDirectory(t)
	res, notices, err := cleanupHiddenSession(t, repo, w)
	assertTempStore(t, before)
	if !exists(w.Path) || !exists(session) || !refExists(repo, "refs/heads/"+w.Branch) {
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
	sandbox(t)
	link := hiddenSessionDirectory(t)
	info, err := os.Stat(link)
	if err != nil || !info.ModTime().Equal(tempTestTime) {
		t.Fatalf("fixture directory age = %v, %v", info, err)
	}
	_, unverified, err := indexPiSessionsStrict()
	if err != nil || len(unverified) != 1 {
		t.Fatalf("symlink discovery = %v, %v", unverified, err)
	}
	if unverified[0].Path != link || !unverified[0].ModTime.IsZero() {
		t.Fatalf("symlink directory must have unknown age: %+v", unverified[0])
	}
}

func TestPiSymlinkFileRetainsTargetAge(t *testing.T) {
	for _, age := range []time.Time{tempTestTime, tempNow} {
		t.Run(age.Format(time.RFC3339), func(t *testing.T) {
			sandbox(t)
			target := filepath.Join(t.TempDir(), "target.jsonl")
			writeFile(t, target, "{}\n", 0o600)
			if err := os.Chtimes(target, age, age); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(piAgentDir(), "sessions", "linked.jsonl")
			mustSymlink(t, target, link)
			_, unverified, err := indexPiSessionsStrict()
			if err != nil || len(unverified) != 1 || !unverified[0].ModTime.Equal(age) {
				t.Fatalf("file symlink target age = %+v, %v; want %v", unverified, err, age)
			}
		})
	}
}
