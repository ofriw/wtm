package main

import (
	"path/filepath"
	"testing"
	"time"
)

func backdateActivityIndex(t *testing.T, path string, when time.Time) string {
	t.Helper()
	dir, err := gitPrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, "index")
	setMtime(t, index, when)
	return index
}

func backdateGitActivity(t *testing.T, path string, when time.Time) {
	t.Helper()
	backdateActivityIndex(t, path, when)
	setMtime(t, filepath.Join(path, ".git"), when)
}

func stageTempActivity(t *testing.T, w worktree, kind string) string {
	t.Helper()
	path := filepath.Join(w.Path, "tracked.txt")
	if kind == "new" {
		path = filepath.Join(w.Path, "new.txt")
	}
	if kind == "delete" {
		mustGit(t, w.Path, "rm", "tracked.txt")
		return ""
	}
	writeFile(t, path, "staged activity", 0600)
	mustGit(t, w.Path, "add", "--", path)
	return path
}

func pinStagedActivity(t *testing.T, w worktree, kind, source string) {
	t.Helper()
	path := stageTempActivity(t, w, kind)
	recent := tempNow.Add(-30 * time.Minute)
	fileTime, indexTime := recent, tempTestTime
	if source == "index" {
		fileTime, indexTime = tempTestTime, recent
	}
	if path != "" {
		setMtime(t, path, fileTime)
	}
	backdateActivityIndex(t, w.Path, indexTime)
}
