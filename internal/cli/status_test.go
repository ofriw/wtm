package cli

import (
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/git"
	"wtm/internal/harness"
	"wtm/internal/temp"
	. "wtm/internal/testutil"
	"wtm/internal/ui"
)

// status_test.go — Tier-1 contracts for status.go. Discovery runs against a
// real repo with linked worktrees; time-based helpers pin RELATIVE mtimes so
// assertions never race the wall clock.

// status tests use the shared defaultHooks() from cli.go so they exercise the
// production session/MCP paths the real dispatcher wires.
func TestWorktreeUnused(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	cases := []struct {
		name     string
		lastUsed time.Time
		want     bool
	}{
		{"zero LastUsed is unused", time.Time{}, true},
		{"younger than ttl is in use", now.Add(-time.Hour), false},
		{"exactly ttl is still in use (cutoff is exclusive)", now.Add(-ttl), false},
		{"older than ttl is unused", now.Add(-ttl - time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := core.Worktree{LastUsed: tc.lastUsed}
			if got := w.Unused(now, ttl); got != tc.want {
				t.Fatalf("unused(now, %v) = %v, want %v", ttl, got, tc.want)
			}
		})
	}
}

func TestWorktreeStatus(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	cases := []struct {
		name     string
		lastUsed time.Time
		want     string
	}{
		{"zero is UNUSED", time.Time{}, "UNUSED"},
		{"recent is ACTIVE", now.Add(-time.Hour), "ACTIVE"},
		{"exactly ttl is ACTIVE", now.Add(-ttl), "ACTIVE"},
		{"older than ttl is UNUSED", now.Add(-ttl - time.Second), "UNUSED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := core.Worktree{LastUsed: tc.lastUsed}
			if got := w.Status(now, ttl); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBranchName(t *testing.T) {
	if got := (core.Worktree{}).BranchName(); got != "(detached)" {
		t.Fatalf("empty branch = %q, want (detached)", got)
	}
	if got := (core.Worktree{Branch: "main"}).BranchName(); got != "main" {
		t.Fatalf("named branch = %q, want main", got)
	}
}

func TestDiscover(t *testing.T) {
	Sandbox(t)
	now := time.Now()
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	linkedA := CanonicalPath(filepath.Join(filepath.Dir(repo), "linked-a"))
	linkedB := CanonicalPath(filepath.Join(filepath.Dir(repo), "linked-b"))
	GitWorktreeAdd(t, repo, linkedA, "feat-a", "refs/heads/main")
	GitWorktreeAdd(t, repo, linkedB, "feat-b", "refs/heads/main")

	// Inventory: main has config+db, linked-a has mcp, linked-b has none.
	WriteFile(t, filepath.Join(repo, harness.ChunkhoundConfigFile), "{}", 0o644)
	WriteFile(t, harness.ChunkhoundDBPath(repo), "db", 0o644)
	WriteFile(t, filepath.Join(linkedA, harness.ProjectMCPFile), "{}", 0o644)
	MkSession(t, repo, "main-s", now.Add(-3*time.Hour))
	MkSession(t, linkedA, "a-s", now.Add(-time.Hour))
	MkSession(t, linkedB, "b-s", now.Add(-2*time.Hour))

	// Pin non-session activity old so the sessions above decide ordering.
	for _, p := range []string{repo, linkedA, linkedB} {
		BackdateGitActivity(t, p, now.Add(-10*time.Hour))
	}
	SetMtime(t, filepath.Join(repo, harness.ChunkhoundConfigFile), now.Add(-10*time.Hour))
	SetMtime(t, harness.ChunkhoundDBPath(repo), now.Add(-10*time.Hour))
	SetMtime(t, filepath.Join(linkedA, harness.ProjectMCPFile), now.Add(-10*time.Hour))

	ws, err := core.LoadWorkspace(&core.Globals{Root: repo}, false, defaultHooks())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	wts := ws.Wts
	wantOrder := []string{CanonicalPath(repo), linkedA, linkedB}
	if len(wts) != len(wantOrder) {
		t.Fatalf("discover returned %d worktrees, want %d", len(wts), len(wantOrder))
	}
	for i, p := range wantOrder {
		if wts[i].Path != p {
			t.Fatalf("order[%d] = %q, want %q", i, wts[i].Path, p)
		}
	}
	if !wts[0].Main || wts[1].Main || wts[2].Main {
		t.Fatalf("Main flag = %v/%v/%v, want true/false/false", wts[0].Main, wts[1].Main, wts[2].Main)
	}
	if wts[0].Caps["chunkhound"] != core.CapPresent || wts[0].Caps["mcp"] != core.CapAbsent {
		t.Fatalf("main inventory = %v", wts[0].Caps)
	}
	if wts[1].Caps["mcp"] != core.CapPresent || wts[1].Caps["chunkhound"] != core.CapAbsent {
		t.Fatalf("linked-a inventory = %v", wts[1].Caps)
	}
	if wts[2].Caps["chunkhound"] != core.CapAbsent || wts[2].Caps["mcp"] != core.CapAbsent {
		t.Fatalf("linked-b must have empty inventory: %v", wts[2].Caps)
	}
}

// TestDiscoverUpstreamsUnavailableReachesNotice pins that when branchUpstreams
// fails, discovery degrades instead of aborting, and the warning reaches stderr
// directly when no renderer runs. The renderer's persistent-notice path is
// pinned separately by TestDiscoverUpstreamsUnavailablePTY on a real terminal.
func TestDiscoverUpstreamsUnavailableReachesNotice(t *testing.T) {
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	// A corrupt packed-refs makes for-each-ref fail while `worktree list` still
	// succeeds, so the branchUpstreams error path is reachable with real git and
	// no stub.
	WriteFile(t, filepath.Join(repo, ".git", "packed-refs"), "garbage\n", 0o644)

	// Non-renderer path: the warning reaches stderr directly.
	stderr := CaptureStderr(t, func() {
		if err := cmdStatus(&core.Globals{Root: repo}, nil, defaultHooks()); err != nil {
			t.Fatalf("cmdStatus: %v", err)
		}
	})
	if !strings.Contains(stderr, "upstreams unavailable") {
		t.Fatalf("nil progress must warn on stderr: %q", stderr)
	}
}

// statusJSON runs the status command in JSON mode and parses its output, so a
// test exercises discovery wiring through the real handler, not a test-only
// seam.
func statusJSON(t *testing.T, repo string) []core.JSONWorktree {
	t.Helper()
	out := CaptureStdout(t, func() {
		if err := cmdStatus(&core.Globals{Root: repo, JSON: true}, nil, defaultHooks()); err != nil {
			t.Fatalf("status --json: %v", err)
		}
	})
	var got []core.JSONWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse status JSON: %v\n%s", err, out)
	}
	return got
}

// TestBuildWorktreeMCPFiles proves each recognized MCP config lights the
// mcp integration equally, plus the harness capability that owns it, keyed off
// mcpConfigFiles as the SSOT. Pinning the exact light-up pairs catches a swap
// that an "either capability" check would miss.
func TestBuildWorktreeMCPFiles(t *testing.T) {
	Sandbox(t) // a developer's real HOME settings must not fail discovery
	empty := statusJSON(t, InitRepo(t, "main"))
	if len(empty) != 1 {
		t.Fatalf("status JSON returned %d worktrees, want 1", len(empty))
	}
	for _, c := range core.Capabilities() {
		if empty[0].Integrations[c.ID] != "n/a" {
			t.Fatalf("empty root: %s = %q, want n/a", c.ID, empty[0].Integrations[c.ID])
		}
	}
	// Every MCP config lights mcp; it also lights the harness that reads it.
	owners := map[string][]string{harness.ProjectMCPFile: {"mcp"}, harness.PiNativeMCPFile: {"mcp", "pi"}}
	for _, rel := range harness.MCPConfigFiles() {
		t.Run(rel, func(t *testing.T) {
			want, ok := owners[rel]
			if !ok {
				t.Fatalf("no capability light-up pinned for %s", rel)
			}
			repo := InitRepo(t, "main")
			WriteFile(t, filepath.Join(repo, rel), "{}", 0o644)
			got := statusJSON(t, repo)
			if len(got) != 1 {
				t.Fatalf("status JSON returned %d worktrees", len(got))
			}
			integ := got[0].Integrations
			for _, c := range []string{"mcp", "pi"} {
				wantState := "n/a"
				if slices.Contains(want, c) {
					wantState = "yes"
				}
				if integ[c] != wantState {
					t.Fatalf("%s: %s = %q, want %q", rel, c, integ[c], wantState)
				}
			}
		})
	}
}

// TestDisplayPath pins the only user-visible path rewrite: $HOME becomes ~.
func TestDisplayPath(t *testing.T) {
	home := t.TempDir()
	SetHomeEnv(t, home)
	sep := string(os.PathSeparator)
	cases := []struct{ in, want string }{
		{home, "~"},
		{home + sep + "repo", "~" + sep + "repo"},
		{home + "x" + sep + "repo", home + "x" + sep + "repo"},
		{sep + "elsewhere", sep + "elsewhere"},
	}
	for _, tc := range cases {
		if got := core.DisplayPath(tc.in); got != tc.want {
			t.Errorf("core.DisplayPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrintTable(t *testing.T) {
	t.Setenv("COLUMNS", "0") // non-TTY goldens must stay at natural widths
	headers := []string{"A", "BB"}
	rows := [][]string{{"x", "y"}, {"long", "z"}}
	out := CaptureStdout(t, func() { ui.PrintTable(headers, nil, rows, ui.ColorAuto, nil, nil, ui.Policy{}) })
	want := "A     BB\nx     y\nlong  z\n"
	if out != want {
		t.Fatalf("printTable = %q, want %q", out, want)
	}
}

// TestStatusHeadersSSOT pins the status header literal and proves the shrink
// policy is derived from the same registry: a rename or a new rank cannot pass
// without updating this contract. It also pins that every column carries a
// stable, unique style-routing ID, so cli's ID-keyed styling stays bound to
// the schema.
func TestStatusHeadersSSOT(t *testing.T) {
	want := []string{"PATH", "BRANCH", "UPSTREAM", "LAST USED", "STATUS", "TEMP", "INTEGRATIONS"}
	if !slices.Equal(core.ColumnHeaders(core.StatusColumns()), want) {
		t.Fatalf("status headers = %v, want %v", core.ColumnHeaders(core.StatusColumns()), want)
	}
	seen := map[string]bool{}
	for _, c := range core.AllColumns() {
		if c.ID == "" {
			t.Errorf("column %q has no style ID", c.Header)
		}
		if seen[c.ID] {
			t.Errorf("column ID %q is not unique", c.ID)
		}
		seen[c.ID] = true
	}
	for _, c := range core.Capabilities() {
		if c.Column().ID != c.ID {
			t.Errorf("capability column ID = %q, want %q", c.Column().ID, c.ID)
		}
	}
	// The elision order production derives for this table is a contract, not an
	// implementation detail.
	priority := core.GridPolicy(want).Priority
	wantShrink := []string{"PATH", "UPSTREAM", "BRANCH", "LAST USED", "INTEGRATIONS"}
	if !slices.Equal(priority, wantShrink) {
		t.Errorf("shrink priority = %v, want %v", priority, wantShrink)
	}
	// Every column a grid uses must be registered in allColumns, or it silently
	// escapes the shared shrink policy.
	registered := map[string]bool{}
	for _, c := range core.AllColumns() {
		registered[c.Header] = true
	}
	for _, cols := range [][]core.WorktreeColumn{core.StatusColumns(), core.PickerColumns()} {
		for _, c := range cols {
			if !registered[c.Header] {
				t.Errorf("column %q is used by a grid but missing from allColumns", c.Header)
			}
		}
	}
}

// TestWideColumnsShareCorePrefix pins that --wide reuses the shared core
// prefix, so a new core column can never silently vanish from the wide view.
func TestWideColumnsShareCorePrefix(t *testing.T) {
	want := core.ColumnHeaders(core.CoreColumns())
	wide := core.ColumnHeaders(core.StatusColumnsFor(true))
	if len(wide) < len(want) || !slices.Equal(wide[:len(want)], want) {
		t.Fatalf("--wide core prefix = %v, want %v", wide, want)
	}
}

// TestWideColumnsShrinkable pins that --wide capability columns participate in
// the shrink policy: they are in the column registry, have a shrink rank, and
// appear in the dynamic shrink priority alongside core columns.
func TestWideColumnsShrinkable(t *testing.T) {
	wideCols := core.StatusColumnsFor(true)
	registry := core.ColumnRegistry()
	for _, c := range wideCols {
		if _, ok := registry[c.Header]; !ok {
			t.Errorf("--wide column %q is not in columnRegistry", c.Header)
		}
	}
	// Capability columns must have a shrink rank so they elide in narrow
	// terminals instead of overflowing the budget.
	for _, c := range core.Capabilities() {
		col := c.Column()
		if col.ShrinkRank == 0 {
			t.Errorf("capability column %q has shrinkRank 0 (pinned); want > 0", col.Header)
		}
		if _, ok := registry[col.Header]; !ok {
			t.Errorf("capability column %q is not in columnRegistry", col.Header)
		}
	}
	// Dynamic shrink priority must include capability columns when present.
	headers := core.ColumnHeaders(wideCols)
	priority := core.GridPolicy(headers).Priority
	for _, c := range core.Capabilities() {
		if !slices.Contains(priority, strings.ToUpper(c.ID)) {
			t.Errorf("capability column %q not in dynamic shrink priority", strings.ToUpper(c.ID))
		}
	}
}

// TestRenderGridMatchesPrintTable pins the extraction: printTable's bytes must
// equal the shared grid engine's header+body rejoined under a no-color profile.
func TestRenderGridMatchesPrintTable(t *testing.T) {
	t.Setenv("COLUMNS", "0")
	headers := []string{"A", "BB", "CCC"}
	rows := [][]string{{"x", "yy", "zzz"}, {"longer", "y", "z"}}
	out := CaptureStdout(t, func() { ui.PrintTable(headers, nil, rows, ui.ColorNever, nil, []string{"CCC"}, ui.Policy{}) })
	h, _, r, widths, aligns := ui.SizedGrid(headers, nil, rows, ui.WidthBudget(), []string{"CCC"}, ui.Policy{})
	header, body := ui.RenderGrid(h, r, widths, aligns)
	if got := ui.GridString(header, body) + "\n"; got != out {
		t.Fatalf("renderGrid = %q, printTable = %q", got, out)
	}
}

// TestPrintStatusTableFits proves the whole status table honors a pinned budget
// end to end, and that core.DisplayPath runs before width budgeting.
func TestPrintStatusTableFits(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	home := t.TempDir()
	SetHomeEnv(t, home)
	wts := []core.Worktree{{
		Path:   filepath.Join(home, "very", "long", "checkout", "that", "never", "ends", "repo"),
		Branch: "feature/a-rather-long-branch-name", LastUsed: time.Now(),
		Upstream: git.Upstream{Short: "origin/feature/a-rather-long-branch-name", Remote: "origin", Ref: "refs/heads/x"},
		Caps:     map[string]core.CapState{"chunkhound": core.CapPresent, "mcp": core.CapPresent},
	}}
	out := CaptureStdout(t, func() { printStatusTable(core.StatusColumns(), wts, time.Now(), 10*time.Hour, ui.ColorNever) })
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := ui.DispWidth(line); w > 80 {
			t.Errorf("line exceeds 80 cells: %d\n%q", w, line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("long path must be middle-elided:\n%s", out)
	}
	if strings.Contains(out, home) {
		t.Fatalf("core.DisplayPath must rewrite $HOME before budgeting:\n%s", out)
	}
	if !strings.Contains(out, "~") {
		t.Fatalf("core.DisplayPath must produce ~ in output:\n%s", out)
	}
}

func TestPrintStatusJSON(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	last := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	wts := []core.Worktree{
		{Path: "/detached", LastUsed: last, Caps: map[string]core.CapState{"chunkhound": core.CapPartial, "mcp": core.CapPresent, "pi": core.CapPresent, "claude": core.CapPresent}},
		{Path: "/main", Branch: "main", Main: true, Temp: true, TempCreated: now, TempTTL: ttl, Caps: map[string]core.CapState{"chunkhound": core.CapPartial}, Upstream: git.Upstream{Short: "origin/main", Remote: "origin", Ref: "refs/heads/main"}},
	}
	out := CaptureStdout(t, func() {
		if err := core.PrintStatusJSON(wts, now, ttl); err != nil {
			t.Errorf("core.PrintStatusJSON: %v", err)
		}
	})
	if !strings.Contains(out, `"lastUsed": null`) {
		t.Fatalf("zero LastUsed must serialize as null: %s", out)
	}
	var got []core.JSONWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("got %d worktrees, want 2", len(got))
	}
	if got[0].Branch != "(detached)" {
		t.Fatalf("detached branch = %q, want (detached)", got[0].Branch)
	}
	if got[0].LastUsed == nil || *got[0].LastUsed != "2021-02-03T04:05:06Z" {
		t.Fatalf("RFC3339 UTC lastUsed = %v, want 2021-02-03T04:05:06Z", got[0].LastUsed)
	}
	if got[0].Status != "UNUSED" {
		t.Fatalf("stale status = %q, want UNUSED", got[0].Status)
	}
	if got[1].LastUsed != nil {
		t.Fatalf("zero LastUsed = %v, want nil", *got[1].LastUsed)
	}
	if !got[1].Main {
		t.Fatalf("Main lost in JSON: %+v", got)
	}
	if got[0].Temp || got[0].ExpiresAt != nil || !got[1].Temp || got[1].ExpiresAt == nil || *got[1].ExpiresAt != temp.FormatDeadline(wts[1].TempExpiresAt()) {
		t.Fatalf("temp metadata lost in JSON: %+v", got)
	}
	if got[1].Upstream != "origin/main" || got[0].Upstream != "" {
		t.Fatalf("upstream lost in JSON: %+v", got)
	}
	// The integrations contract: present->yes, partial->partial, an applicable
	// absence->-. Expectation is derived from core.Capabilities() so registering a
	// new integration needs no test edit; the pinned state->word mapping is the
	// contract. wts[0] carries mcp/pi/claude, so they are applicable (not n/a)
	// and read "-" for wts[1].
	integrations := func(state map[string]core.CapState) map[string]string {
		want := make(map[string]string, len(core.Capabilities()))
		for _, c := range core.Capabilities() {
			want[c.ID] = "-"
			switch state[c.ID] {
			case core.CapPresent:
				want[c.ID] = "yes"
			case core.CapPartial:
				want[c.ID] = "partial"
			}
		}
		return want
	}
	wantDetached := integrations(wts[0].Caps)
	wantMain := integrations(wts[1].Caps)
	if !maps.Equal(got[0].Integrations, wantDetached) {
		t.Fatalf("detached integrations = %v, want %v", got[0].Integrations, wantDetached)
	}
	if !maps.Equal(got[1].Integrations, wantMain) {
		t.Fatalf("main integrations = %v, want %v", got[1].Integrations, wantMain)
	}
	// Field names are the script contract: decode to maps and assert the exact
	// key set, which also proves the removed config/db/mcp/mcpNative keys stay
	// gone even when the struct tags follow.
	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("re-decode JSON: %v", err)
	}
	wantKeys := []string{"branch", "expiresAt", "integrations", "lastUsed", "main", "path", "status", "temp", "upstream"}
	for i, m := range raw {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, wantKeys) {
			t.Fatalf("row %d keys = %v, want %v", i, keys, wantKeys)
		}
	}
}

// TestDispatchValidatesColor pins Fix C: --color is validated once in
// dispatch, so every subcommand rejects a bogus value as a usage error (exit 2)
// before touching the repo.
func TestDispatchValidatesColor(t *testing.T) {
	for _, cmd := range []string{"status", "add", "delete", "gc", "config"} {
		g := &core.Globals{Color: "bogus"}
		if err := dispatch(cmd, g, nil); !isUsage(err) {
			t.Errorf("dispatch(%q,Color:bogus) = %v, want usageError", cmd, err)
		}
	}
}

// TestParseSubRejectsInvalidGlobalsAfterSubcommand pins that a bogus global
// placed AFTER the subcommand is validated: subFlags re-binds globals, so the
// value is only visible once parseSub has parsed it.
func TestParseSubRejectsInvalidGlobalsAfterSubcommand(t *testing.T) {
	for _, args := range [][]string{
		{"--color", "bogus"},
		{"--progress", "sometimes"},
	} {
		if err := cmdStatus(&core.Globals{Root: t.TempDir()}, args, defaultHooks()); !isUsage(err) {
			t.Errorf("cmdStatus(%v) = %v, want usageError", args, err)
		}
	}
}
func TestTempDeadlineStatusAndSelectionAgree(t *testing.T) {
	w := core.Worktree{Temp: true, TempCreated: TempTestTime, LastUsed: TempTestTime.Add(time.Second), TempTTL: 1500 * time.Millisecond}
	deadline := TempTestTime.Add(2500 * time.Millisecond)
	if got := w.TempExpiresAt(); !got.Equal(deadline) {
		t.Fatalf("deadline = %s, want %s", got, deadline)
	}
	if w.Unused(deadline, time.Hour) || !w.Unused(deadline.Add(time.Nanosecond), time.Hour) {
		t.Fatal("status and selection disagree at the fractional deadline")
	}
}

// TestGCPickerExcludesIntegrations pins that the gc picker never shows the
// INTEGRATIONS column: the picker builds columnContext without active, so the
// grouped cell would misreport every worktree as "-".
func TestGCPickerExcludesIntegrations(t *testing.T) {
	for _, c := range core.PickerColumns() {
		if c.Header == "INTEGRATIONS" {
			t.Fatalf("picker must not contain %q", "INTEGRATIONS")
		}
	}
}

// TestWideColumnOrder pins the --wide column order: project integrations first
// (chunkhound, mcp), then agents in agents() order (pi, claude).
func TestWideColumnOrder(t *testing.T) {
	wide := core.StatusColumnsFor(true)
	var headers []string
	for _, c := range wide {
		headers = append(headers, c.Header)
	}
	coreHdrs := core.ColumnHeaders(core.CoreColumns())
	if len(headers) < len(coreHdrs) || !slices.Equal(headers[:len(coreHdrs)], coreHdrs) {
		t.Fatalf("--wide core prefix = %v, want %v", headers[:len(coreHdrs)], coreHdrs)
	}
	var caps []string
	for _, c := range core.Capabilities() {
		caps = append(caps, strings.ToUpper(c.ID))
	}
	for i, want := range caps {
		if headers[len(coreHdrs)+i] != want {
			t.Fatalf("--wide cap[%d] = %q, want %q", i, headers[len(coreHdrs)+i], want)
		}
	}
}

// TestStatusColumnsForNoAliasing pins that the default schema is never returned
// by reference: callers must not be able to mutate the global core.StatusColumns.
func TestStatusColumnsForNoAliasing(t *testing.T) {
	got := core.StatusColumnsFor(false)
	got[0] = core.WorktreeColumn{Header: "PAD"}
	fresh := core.ColumnHeaders(core.StatusColumns())
	if !slices.Equal(fresh, core.ColumnHeaders(core.StatusColumnsFor(false))) {
		t.Fatalf("core.StatusColumns mutated: %v", fresh)
	}
	if fresh[0] == "PAD" {
		t.Fatalf("fresh schema carries mutation: %v", fresh)
	}
}

// TestWithProgressGatedOff pins the gating half of the orchestration: with the
// renderer gated off by --progress never, work gets the nil-safe handle and no
// renderer runs.
func TestWithProgressGatedOff(t *testing.T) {
	g := &core.Globals{Progress: ui.ProgressNever}
	ran := false
	err := withProgressTo(io.Discard, g, true, func(p *ui.Progress) error {
		ran = p == nil
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("withProgressTo = (%v, nil-handle=%v), want (nil, true)", err, ran)
	}
}
