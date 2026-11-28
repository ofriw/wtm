package main

import (
	"bufio"
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fixes_review_a_test.go — regressions for the Phase A correctness fixes from
// the PR #2 review: cancel must not arm the rate cooldown, usage-bearing
// records must never vanish without tainting, control chars are stripped from
// display labels, fold sums saturate, and every session-read cause is disclosed.

// TestRatesCancelDoesNotArmCooldown pins that a user-cancelled fetch returns
// the cancellation (not a transport failure), writes no cooldown marker, and
// lets the very next run fetch immediately.
func TestRatesCancelDoesNotArmCooldown(t *testing.T) {
	path := isolatedRatesCache(t)
	arrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan progressEvent, 16)
	p := &progress{ctx: ctx, events: events}
	done := make(chan struct{})
	var gotErr error
	go func() {
		_, gotErr = loadCatalogFromNotify(srv.URL, p, time.Now())
		close(done)
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch request never reached the server")
	}
	cancel()
	<-done
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("loadCatalogFromNotify err = %v, want context.Canceled", gotErr)
	}
	if _, serr := os.Lstat(attemptPath(path)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("cooldown marker written despite cancellation: %v", serr)
	}
	// The next run must retry immediately and succeed.
	url, hits := serveRatesCounting(t, http.StatusOK, testCatalog)
	c, err := loadCatalogFrom(url)
	if err != nil {
		t.Fatalf("immediate retry failed: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Fatal("retry served no catalog")
	}
	if hits.Load() != 1 {
		t.Errorf("retry fetches = %d, want 1", hits.Load())
	}
}

// TestRatesDeadlineDoesNotArmCooldown pins the timeout shape of the same
// guard: a run whose context deadline expires mid-fetch must not arm a
// cooldown, so the next run retries immediately.
func TestRatesDeadlineDoesNotArmCooldown(t *testing.T) {
	path := isolatedRatesCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	events := make(chan progressEvent, 16)
	p := &progress{ctx: ctx, events: events}
	_, err := loadCatalogFromNotify(srv.URL, p, time.Now())
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("loadCatalogFromNotify err = %v, want a context deadline/cancel error", err)
	}
	if _, serr := os.Lstat(attemptPath(path)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("cooldown marker written despite deadline: %v", serr)
	}
	url, hits := serveRatesCounting(t, http.StatusOK, testCatalog)
	if _, err := loadCatalogFrom(url); err != nil {
		t.Fatalf("immediate retry failed: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("retry fetches = %d, want 1", hits.Load())
	}
}

// TestPromptTokensPlusSaturates pins that fold sums cannot wrap int64 and flip
// evidence to absent.
func TestPromptTokensPlusSaturates(t *testing.T) {
	a := promptTokens{input: math.MaxInt64 - 1, output: math.MaxInt64}
	b := promptTokens{input: 5, output: 1}
	got := a.plus(b)
	if got.input != math.MaxInt64 {
		t.Errorf("input = %d, want saturating MaxInt64", got.input)
	}
	if got.output != math.MaxInt64 {
		t.Errorf("output = %d, want saturating MaxInt64", got.output)
	}
}

// TestUsageLossReportsEveryCause pins that distinct session-read causes are all
// disclosed, not hidden behind the first.
func TestUsageLossReportsEveryCause(t *testing.T) {
	l := newUsageLoss()
	l.fail("claude", errors.New("cause one"))
	l.fail("pi", errors.New("cause two"))
	l.fail("claude", errors.New("cause one")) // identical text collapses
	err := usageLossErr(l.errs, l.dropped, l.unsupported)
	if err == nil {
		t.Fatal("usageLossErr = nil, want both causes")
	}
	s := err.Error()
	if !strings.Contains(s, "cause one") || !strings.Contains(s, "cause two") {
		t.Fatalf("usageLossErr = %q, want both distinct causes", s)
	}
}

// TestClaudeRecordLevelUsageTaints pins that an assistant record routed for
// billing whose payload has no usable message usage taints the total instead of
// vanishing in silence.
func TestClaudeRecordLevelUsageTaints(t *testing.T) {
	line := `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":"r1","message":null,"usage":{"input_tokens":1000}}`
	out, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(line + "\n")))
	if err != nil {
		t.Fatalf("claudeUsageEvents: %v", err)
	}
	if len(out.events) != 0 {
		t.Fatalf("events = %d, want 0 for an unusable payload", len(out.events))
	}
	if out.dropped != 1 {
		t.Errorf("dropped = %d, want 1 (record-level usage must taint)", out.dropped)
	}
}

// TestPiMessageRecordLevelUsageBills pins that a message record carrying
// top-level usage (with no message body) still bills, matching the "usage"
// record handling.
func TestPiMessageRecordLevelUsageBills(t *testing.T) {
	line := `{"type":"message","id":"e1","timestamp":"2026-01-01T00:00:00Z","message":null,"usage":{"input":100,"output":50}}`
	out, err := piUsageEvents(bufio.NewReader(strings.NewReader(line + "\n")))
	if err != nil {
		t.Fatalf("piUsageEvents: %v", err)
	}
	if len(out.events) != 1 {
		t.Fatalf("events = %d, want 1", len(out.events))
	}
	e := out.events[0]
	if e.tokens.input != 100 || e.tokens.output != 50 {
		t.Errorf("tokens = %d/%d, want 100/50", e.tokens.input, e.tokens.output)
	}
}

// TestDetailRowStripsControlChars pins that a model label carrying a literal
// control byte renders without it.
func TestDetailRowStripsControlChars(t *testing.T) {
	m := &modelUsage{cost: costPair{billed: money{any: true}}, spans: []span{}}
	row := detailRow("evil\x08name", m, false)
	if strings.Contains(row[0], "\x08") {
		t.Fatalf("control byte survived rendering: %q", row[0])
	}
	if row[0] != "evilname" {
		t.Errorf("row[0] = %q, want %q", row[0], "evilname")
	}
}

// TestDetailRowStripsANSIBeforeControls ensures stripping order: ANSI escape
// sequences are removed first, then residual control bytes.
func TestDetailRowStripsANSIBeforeControls(t *testing.T) {
	m := &modelUsage{cost: costPair{billed: money{any: true}}, spans: []span{}}
	row := detailRow("\x1b[31mred\x1b[0m\x08", m, false)
	if strings.Contains(row[0], "\x1b") || strings.Contains(row[0], "\x08") {
		t.Fatalf("escape or control byte survived: %q", row[0])
	}
	if row[0] != "red" {
		t.Errorf("row[0] = %q, want %q", row[0], "red")
	}
}

// TestClaudeUsageLineFixtureGuard keeps the para fixture honest: the line shape
// the record-level-usage test relies on must keep parser as assistant-with-usage.
func TestClaudeUsageLineFixtureGuard(t *testing.T) {
	line := `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":"r1","message":null,"usage":{"input_tokens":1000}}`
	typ, typOK, hasUsage, malformed := scanRecord([]byte(line))
	if !typOK || typ != "assistant" || !hasUsage || malformed {
		t.Fatalf("scanRecord = %q/%v/%v/%v, want assistant/true/true/false", typ, typOK, hasUsage, malformed)
	}
}
