package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddJSONDeadlineMatchesRegistryUnderContention(t *testing.T) {
	sandboxTempProcesses(t)
	repo := addTestRepo(t)
	holder := startTempProcess(t, "hold-lock", repo)
	holder.expect(t, "locked")
	contender := startTempProcess(t, "add-deadline", repo)
	contender.expect(t, "ready")
	assertAddCheckoutBeforeRegistration(t, repo)
	assertTempProcessContention(t, repo)
	releaseTempProcess(t, holder)
	res := readAddDeadlineResult(t, contender)
	assertPersistedAddDeadline(t, res)
}

func assertAddCheckoutBeforeRegistration(t *testing.T, repo string) {
	t.Helper()
	path := addTarget(repo, "tmp/deadline")
	awaitAddCheckout(t, filepath.Join(path, ".git"))
	if _, exists := mustReadTempStore(t)[path]; exists {
		t.Fatal("add registered the checkout while the holder retained the lock")
	}
}

// Checkout presence is observable; it does not imply registration has started.
func awaitAddCheckout(t *testing.T, gitFile string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := os.Stat(gitFile); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("checkout did not appear: %s", gitFile)
		case <-poll.C:
		}
	}
}

func assertPersistedAddDeadline(t *testing.T, res addResult) {
	t.Helper()
	rec := mustReadTempStore(t)[canonical(res.Path)]
	// Recompute from the recorded creation time, not the parent clock: a
	// backward NTP step between record and check is not a contract violation.
	deadline := rec.CreatedAt.Add(1500 * time.Millisecond)
	if res.ExpiresAt == nil || *res.ExpiresAt != deadline.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("add deadline = %v, persisted deadline = %s", res.ExpiresAt, deadline)
	}
	if rec.TTL != "1.5s" || !res.Temp || rec.Identity == "" {
		t.Fatalf("temporary registration missing: result %+v, record %+v", res, rec)
	}
}

func readAddDeadlineResult(t *testing.T, p *tempTestProcess) addResult {
	t.Helper()
	output := readTempProcessResult(t, p)
	var res addResult
	mustProcessError(t, json.Unmarshal([]byte(output), &res))
	p.finish(t)
	return res
}

func readTempProcessResult(t *testing.T, p *tempTestProcess) string {
	t.Helper()
	var output strings.Builder
	for p.output.Scan() {
		if p.output.Text() == "done" {
			break
		}
		output.WriteString(p.output.Text())
		output.WriteByte('\n')
	}
	if p.output.Text() != "done" {
		t.Fatal("add process ended without its completion signal")
	}
	mustProcessError(t, p.output.Err())
	return output.String()
}
