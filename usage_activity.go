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

// activityEvent is one turn boundary or work marker. answer carries an
// assistant record's model: only that explicit answer may own a turn's opening,
// so a warmup or summary usage record (work without an answer) never can.
type activityEvent struct {
	at     time.Time
	kind   activityKind
	answer string
}

// turn is one causal turn and the model that answered it. answerAt is that
// model's first assistant record, which bounds the turn's opening; a turn with
// no assistant record leaves both empty and falls back to its usage events.
type turn struct {
	span
	answer   string
	answerAt time.Time
}

// sortActivity orders turn markers by time. Stable, so records at the same
// instant keep file order: an answer that closes the previous turn at a shared
// boundary stays with that turn.
func sortActivity(events []activityEvent) {
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
}

// answerMark is one assistant record inside the open turn, in file order.
type answerMark struct {
	model string
	at    time.Time
}

// turnState tracks one session's open turn. A human prompt while a turn is open
// closes the previous turn at its last work record, which keeps a missing
// end_turn from counting the user's idle time.
type turnState struct {
	blocks []turn
	open   bool
	start  time.Time
	last   time.Time
	marks  []answerMark
}

func (s *turnState) closeAt(end time.Time) {
	if end.After(s.start) {
		t := turn{span: span{start: s.start, end: end}}
		t.answer, t.answerAt = turnAnswer(s.marks)
		s.blocks = append(s.blocks, t)
	}
	s.open, s.marks = false, nil
}

func (s *turnState) openAt(at time.Time) { s.open, s.start, s.last, s.marks = true, at, at, nil }

func (s *turnState) apply(e activityEvent) {
	switch e.kind {
	case activityHuman:
		if s.open {
			s.closeAt(s.last)
		}
		s.openAt(e.at)
	case activityWork:
		if s.open {
			s.last = maxTime(s.last, e.at)
		} else {
			s.openAt(e.at)
		}
		s.mark(e)
	case activityEnd:
		if s.open {
			s.last = maxTime(s.last, e.at)
			s.mark(e)
			s.closeAt(s.last)
		}
	}
}

// mark records an assistant record; the last one is the turn's answer.
func (s *turnState) mark(e activityEvent) {
	if e.answer != "" {
		s.marks = append(s.marks, answerMark{model: e.answer, at: e.at})
	}
}

// turnAnswer picks the model of the last assistant record and the time of its
// first record, so a tool call early in a multi-record answer still bounds the
// opening. No assistant record means the transcript names no answer.
func turnAnswer(marks []answerMark) (string, time.Time) {
	if len(marks) == 0 {
		return "", time.Time{}
	}
	name := marks[len(marks)-1].model
	for _, m := range marks {
		if m.model == name {
			return name, m.at
		}
	}
	return name, time.Time{}
}

// buildTurns reconstructs one session's turns. Records are sorted by time so
// file order cannot open or close a turn out of sequence. EOF closes at the last
// work record, so trailing idle never counts.
func buildTurns(events []activityEvent) []turn {
	if len(events) == 0 {
		return nil
	}
	sorted := slices.Clone(events)
	sortActivity(sorted)
	var st turnState
	for _, e := range sorted {
		st.apply(e)
	}
	if st.open {
		st.closeAt(st.last)
	}
	return st.blocks
}

// turnSpans projects turns to their intervals; the walk keeps them sorted and
// disjoint.
func turnSpans(turns []turn) []span {
	out := make([]span, len(turns))
	for i, t := range turns {
		out[i] = t.span
	}
	return out
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

// sessionTurns reconstructs the session's complete causal turns without the
// report-clock bound. Attribution needs the full turn so a turn that crosses
// the clock still credits its answering model; the caller clamps the result.
func sessionTurns(activity []activityEvent, events []usageEvent) []turn {
	if len(activity) == 0 {
		activity = activityFromEvents(events)
	}
	return buildTurns(activity)
}

// maxSpanWidth once bounded a span's width as corrupt time. Transcript timestamps
// are plausibility-checked at parse (see transcriptTimestamp), so no span can
// exceed a century; the blanket-drop is unreachable and was silently discarding
// spans without tainting the total. Dropped corrupt records taint the total at
// the parse boundary instead, where the drop is counted and disclosed.

// clampSpans bounds every span by the report clock. A span wholly after now is
// dropped; a straddling span ends at now. It clones its input like mergeSpans,
// so callers keep ownership of the slice they pass.
func clampSpans(spans []span, now time.Time) []span {
	out := make([]span, 0, len(spans))
	for _, s := range spans {
		if s.start.After(now) {
			continue
		}
		if s.end.After(now) {
			s.end = now
		}
		out = append(out, s)
	}
	return out
}

// turnOpenings credits each turn's uncovered opening to the model that answered
// there. The opening ends at the answering model's first assistant record, so a
// warmup or summary usage record can never claim it. A turn whose transcript
// names no assistant model falls back to its first point usage event after the
// turn start; the boundary record stays with the turn it closed. covered is the
// session's merged clipped model time, which an opening may not duplicate.
func turnOpenings(turns []turn, events []usageEvent, covered []span) map[string][]span {
	out := map[string][]span{}
	if len(turns) == 0 {
		return out
	}
	points := pointsForFallback(turns, events)
	for _, t := range turns {
		points = skipThrough(points, t.start)
		model, end := t.answer, t.answerAt
		if model == "" && len(points) > 0 && !points[0].at.After(t.end) {
			model, end = points[0].model, points[0].at
		}
		points = skipThrough(points, t.end)
		if model == "" || !end.After(t.start) {
			continue
		}
		if gaps := subtractSpans(span{start: t.start, end: end}, covered); len(gaps) > 0 {
			out[model] = append(out[model], gaps...)
		}
	}
	return out
}

// pointsForFallback lists the session's point events in time order, but only
// when some turn has no assistant answer; answered sessions pay nothing.
func pointsForFallback(turns []turn, events []usageEvent) []usageEvent {
	if !slices.ContainsFunc(turns, func(t turn) bool { return t.answer == "" }) {
		return nil
	}
	points := make([]usageEvent, 0, len(events))
	for _, e := range events {
		if e.endAt().Equal(e.at) {
			points = append(points, e)
		}
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].at.Before(points[j].at) })
	return points
}

// skipThrough drops points at or before an instant. The boundary record belongs
// to the turn it closed, so the next turn never rescans it.
func skipThrough(points []usageEvent, before time.Time) []usageEvent {
	for len(points) > 0 && !points[0].at.After(before) {
		points = points[1:]
	}
	return points
}

// subtractSpans returns the parts of s not covered by merged, which must be
// merged, sorted and disjoint (see mergeSpans). A binary search finds the first
// candidate, so a turn never rescans the session's whole coverage.
func subtractSpans(s span, merged []span) []span {
	var out []span
	cur := s.start
	i := sort.Search(len(merged), func(i int) bool { return merged[i].end.After(s.start) })
	for ; i < len(merged) && merged[i].start.Before(s.end); i++ {
		if merged[i].start.After(cur) {
			out = append(out, span{start: cur, end: merged[i].start})
		}
		cur = maxTime(cur, merged[i].end)
	}
	if cur.Before(s.end) {
		out = append(out, span{start: cur, end: s.end})
	}
	return out
}

// clipSpans keeps only the parts of raw spans inside the session's active turns.
// Per-model spans keep a spawn child's call-to-result time; clipping removes the
// idle between turns without changing model attribution inside a turn. Both
// sides are merged once, so one long run cannot rescan every turn.
func clipSpans(raw, blocks []span) []span {
	if len(raw) == 0 || len(blocks) == 0 {
		return nil
	}
	return intersectSpans(mergeSpans(raw), mergeSpans(blocks))
}

// intersectSpans intersects two merged, sorted span lists in one walk. A
// zero-length overlap adds no time and no coverage, so it is dropped.
func intersectSpans(spans, blocks []span) []span {
	out := make([]span, 0, len(spans))
	j := 0
	for _, s := range spans {
		for j < len(blocks) && !blocks[j].end.After(s.start) {
			j++
		}
		for k := j; k < len(blocks) && blocks[k].start.Before(s.end); k++ {
			start, end := maxTime(s.start, blocks[k].start), minTime(s.end, blocks[k].end)
			if start.Before(end) {
				out = append(out, span{start: start, end: end})
			}
		}
	}
	return out
}
