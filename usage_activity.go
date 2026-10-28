package main

import (
	"slices"
	"sort"
	"time"
)

// usage_activity.go — the causal turn model behind ACTIVE. A transcript is an
// event log, not a duration log: a gap between records may be a long tool run
// or an idle user. The parsers therefore reconstruct the agent's turn from the
// records themselves: a human prompt opens a turn, tool use and tool results
// keep it open, and an end/error/abort closes it. ACTIVE is the union of those
// turns, so idle between turns never counts and no arbitrary cutoff is needed.

// sessionParse is one session file's parsed facts. events price the session;
// activity reconstructs its turns; dropped counts records that could have
// carried billed usage.
type sessionParse struct {
	events   []usageEvent
	activity []activityEvent
	dropped  int
}

// activityKind is one record's effect on the open agent turn.
type activityKind uint8

const (
	// activityWork keeps a turn open: assistant tool use, a tool result, or an
	// agent-side usage record such as compaction.
	activityWork activityKind = iota
	// activityHuman is a prompt typed by the user; it opens a new turn.
	activityHuman
	// activityEnd is end_turn/error/abort; it closes the open turn.
	activityEnd
)

// activityEvent is one turn boundary or work marker. Model attribution is not
// needed here: per-model TIME clips the usage events' spans to these turns.
type activityEvent struct {
	at   time.Time
	kind activityKind
}

// sessionBlocks reconstructs the agent turns in one session. Records are sorted
// by time so file order cannot open or close a turn out of sequence. A human
// prompt while a turn is open closes the previous turn at its last work record,
// which keeps a missing end_turn from counting the user's idle time. EOF closes
// at the last work record for the same reason.
func sessionBlocks(events []activityEvent) []span {
	if len(events) == 0 {
		return nil
	}
	sorted := slices.Clone(events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].at.Before(sorted[j].at) })
	var blocks []span
	open, start, last := false, time.Time{}, time.Time{}
	closeAt := func(end time.Time) {
		if end.After(start) {
			blocks = append(blocks, span{start: start, end: end})
		}
		open = false
	}
	for _, e := range sorted {
		switch e.kind {
		case activityHuman:
			if open {
				closeAt(last)
			}
			open, start, last = true, e.at, e.at
		case activityWork:
			if !open {
				open, start, last = true, e.at, e.at
				continue
			}
			last = maxTime(last, e.at)
		case activityEnd:
			if !open {
				continue
			}
			last = maxTime(last, e.at)
			closeAt(last)
		}
	}
	if open {
		closeAt(last)
	}
	return blocks
}

// activityFromEvents is the fallback for a caller that has usage events but no
// activity records (tests and older parse paths). It treats the whole event
// range as one turn, preserving the pre-causal behavior rather than inventing
// boundaries the transcript did not record.
func activityFromEvents(events []usageEvent) []activityEvent {
	out := make([]activityEvent, 0, 2*len(events))
	for _, e := range events {
		out = append(out, activityEvent{at: e.at, kind: activityWork})
		if end := e.endAt(); end.After(e.at) {
			out = append(out, activityEvent{at: end, kind: activityWork})
		}
	}
	return out
}

// sessionActivity picks the activity source: the parser's causal records when
// present, else the usage-event fallback.
func sessionActivity(activity []activityEvent, events []usageEvent) []span {
	if len(activity) == 0 {
		activity = activityFromEvents(events)
	}
	return sessionBlocks(activity)
}

// clipSpans keeps only the parts of raw spans that fall inside the session's
// active turns. Per-model spans are built from usage events (so a spawn child
// keeps its call-to-result time); clipping removes the idle between turns
// without changing model attribution inside a turn.
func clipSpans(raw, blocks []span) []span {
	if len(raw) == 0 || len(blocks) == 0 {
		return nil
	}
	out := make([]span, 0, len(raw))
	for _, r := range raw {
		for _, b := range blocks {
			start := maxTime(r.start, b.start)
			end := minTime(r.end, b.end)
			if !end.Before(start) {
				out = append(out, span{start: start, end: end})
			}
		}
	}
	return out
}
