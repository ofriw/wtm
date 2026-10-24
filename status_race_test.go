package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// status_race_test.go — the status usage pool must stay race-free when one
// run-level credential proof is shared by many worktree workers.
//
// External contract only: the test seeds a real repo with worktrees and real Pi
// and Claude session histories, then runs the race-instrumented CLI and checks
// the documented JSON plus the absence of a detector report. A race in a worker
// fails here whichever language implements wtm.

// buildRaceWTM builds the CLI with the race detector. A race in the child is
// invisible to the test process's own -race instrumentation, so the child must
// carry the detector too.
func buildRaceWTM(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "CGO_ENABLED").Output()
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Skip("race-instrumented build needs cgo")
	}
	bin := filepath.Join(t.TempDir(), "wtm-race")
	if out, err := exec.Command("go", "build", "-race", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build -race: %v\n%s", err, out)
	}
	return bin
}

// seedPiHistory writes one Pi session owned by wt with a covered provider.
func seedPiHistory(t *testing.T, agentDir, wt string, i int) {
	t.Helper()
	id := fmt.Sprintf("pi%d", i)
	body := fmt.Sprintf("{\"id\":%q,\"cwd\":%q}\n%s\n", id, wt, piProviderLine("anthropic", 10))
	writeFile(t, filepath.Join(agentDir, "sessions", id, id+".jsonl"), body, 0o644)
}

// seedClaudeHistory writes one Claude session owned by wt with one priced turn.
func seedClaudeHistory(t *testing.T, claudeDir, wt string, i int) {
	t.Helper()
	id := fmt.Sprintf("claude%d", i)
	body := fmt.Sprintf("{\"type\":\"user\",\"cwd\":%q,\"message\":{\"role\":\"user\",\"content\":\"seed\"}}\n", wt) +
		claudeUsageLine(`{"input_tokens":100000}`) + "\n"
	writeFile(t, filepath.Join(claudeDir, "projects", id, id+".jsonl"), body, 0o644)
}

// jsonWT is the documented per-worktree status contract.
type jsonWT struct {
	Path  string `json:"path"`
	Usage map[string]struct {
		Cost     *float64 `json:"cost"`
		ListCost *float64 `json:"listCost"`
	} `json:"usage"`
}

// TestStatusSharedProofRaceFree pins two invariants at once: both harness
// histories price under one shared credential proof, and only the worktree with
// its own metered override loses the Claude proof. Running the
// race-instrumented binary makes a worker that mutates the shared proof fail
// here instead of crashing an operator's terminal.
func TestStatusSharedProofRaceFree(t *testing.T) {
	if testing.Short() {
		t.Skip("race build is slow")
	}
	bin := buildRaceWTM(t) // must precede sandbox: a sandboxed HOME relocates GOMODCACHE
	sb := sandbox(t)
	writeAuthJSON(t, `{"anthropic":{"type":"oauth"}}`)
	writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
	rates := filepath.Join(t.TempDir(), "rates.json")
	writeFile(t, rates, testCatalog, 0o644)

	repo := initRepo(t, "main")
	writeFile(t, filepath.Join(repo, "seed.txt"), "seed\n", 0o644)
	gitCommit(t, repo, "seed", "seed.txt")

	const worktrees = 4
	wts := make([]string, worktrees)
	for i := range wts {
		wts[i] = filepath.Join(t.TempDir(), fmt.Sprintf("wt%d", i))
		gitWorktreeAdd(t, repo, wts[i], fmt.Sprintf("feat%d", i), "main")
		seedPiHistory(t, sb.AgentDir, wts[i], i)
		seedClaudeHistory(t, sb.ClaudeDir, wts[i], i)
	}
	metered := wts[0]
	writeFile(t, filepath.Join(metered, ".claude", "settings.json"), `{"apiKeyHelper":"/bin/fetch"}`, 0o600)

	cmd := exec.Command(bin, "status", "--json")
	cmd.Dir = repo
	cmd.Env = childTerminalEnv([]string{"WTM_RATES=" + rates})
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if strings.Contains(stderr.String(), "DATA RACE") {
		t.Fatalf("status raced on the shared credential proof:\n%s", stderr.String())
	}
	if err != nil {
		t.Fatalf("status --json: %v\nstderr:\n%s", err, stderr.String())
	}

	var got []jsonWT
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	for _, wt := range wts {
		j, ok := findWorktreeJSON(got, wt)
		if !ok {
			t.Fatalf("status lost worktree %s", wt)
		}
		pi, pok := j.Usage["pi"]
		cl, cok := j.Usage["claude"]
		if !pok || !cok {
			t.Fatalf("%s must price both histories: %+v", wt, j.Usage)
		}
		if pi.Cost == nil || *pi.Cost != 0 {
			t.Fatalf("%s pi cost = %v, want covered $0", wt, pi.Cost)
		}
		wantCovered := wt != metered
		if covered := cl.Cost != nil && *cl.Cost == 0; covered != wantCovered {
			t.Fatalf("%s claude cost = %v, covered = %v, want %v", wt, cl.Cost, covered, wantCovered)
		}
		if wantCovered {
			// A covered turn keeps its list price beside the discounted billed
			// value; an unproven turn bills the list, so the pair collapses.
			if cl.ListCost == nil || *cl.ListCost <= 0 {
				t.Fatalf("%s claude listCost = %v, want the list price", wt, cl.ListCost)
			}
		}
	}
}

// findWorktreeJSON matches the display path by its final element, because
// status may shorten a path under HOME.
func findWorktreeJSON(wts []jsonWT, path string) (jsonWT, bool) {
	base := filepath.Base(path)
	for _, w := range wts {
		if filepath.Base(w.Path) == base {
			return w, true
		}
	}
	return jsonWT{}, false
}
