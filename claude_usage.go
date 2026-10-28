package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
)

// claude_usage.go — the Claude Code usage parser. Claude writes tokens, never a
// cost, and writes one API turn as several streaming records that share a
// message id and repeat the usage object. The parser emits one candidate event
// per assistant record and tags it with the turn's identity; the worktree-level
// dedupe (dedupeKeyedEvents) keeps the occurrence with the best score (tokens,
// then web-search count, then self-priced cost), so a turn counts once even when
// a resumed or forked session file repeats it. The same walk reconstructs the
// causal turn for ACTIVE: a human prompt opens it, tool use/results keep it
// open, and end_turn/error closes it.

type claudeUsageRecord struct {
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	RequestID string         `json:"requestId"`
	Message   *claudeMessage `json:"message"`
}

type claudeMessage struct {
	ID         string       `json:"id"`
	Model      string       `json:"model"`
	StopReason string       `json:"stop_reason"`
	Usage      *claudeUsage `json:"usage"`
}

// claudeActivityRecord is the lightweight shape for records with a turn
// boundary but no billable usage. Content stays raw; isMeta is top-level.
type claudeActivityRecord struct {
	Type      string                 `json:"type"`
	Timestamp string                 `json:"timestamp"`
	IsMeta    bool                   `json:"isMeta"`
	Message   *claudeActivityMessage `json:"message"`
}

type claudeActivityMessage struct {
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
}

type claudeUsage struct {
	InputTokens   *int64               `json:"input_tokens"`
	OutputTokens  *int64               `json:"output_tokens"`
	CacheRead     *int64               `json:"cache_read_input_tokens"`
	CacheCreation *int64               `json:"cache_creation_input_tokens"`
	CacheDetail   *claudeCacheCreation `json:"cache_creation"`
	ServiceTier   *string              `json:"service_tier"`
	ServerToolUse *claudeServerToolUse `json:"server_tool_use"`
}

type claudeCacheCreation struct {
	Ephemeral5m *int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h *int64 `json:"ephemeral_1h_input_tokens"`
}

type claudeServerToolUse struct {
	WebSearchRequests *int64 `json:"web_search_requests"`
}

// claudeUsageEvents parses one Claude session into cost events and causal turn
// activity. Unreadable records are counted so the caller can warn.
func claudeUsageEvents(r *bufio.Reader) (sessionParse, error) {
	var out sessionParse
	for {
		line, oversized, prefix, err := sessionRecord(r)
		out.consumeClaudeLine(line, oversized, prefix)
		if err != nil {
			if err == io.EOF {
				out.sortClaude()
				return out, nil
			}
			// A read error must not discard the records already parsed: the
			// caller drops the session (and taints the total), but the kept
			// events stay available for diagnostics and tests.
			out.sortClaude()
			return out, err
		}
	}
}

func (p *sessionParse) sortClaude() {
	sortUsageEvents(p.events)
	sort.SliceStable(p.activity, func(i, j int) bool { return p.activity[i].at.Before(p.activity[j].at) })
}

// consumeClaudeLine folds one record: a billed assistant drives cost, and any
// user/assistant record drives the turn state.
func (p *sessionParse) consumeClaudeLine(line []byte, oversized bool, prefix []byte) {
	if oversized {
		if oversizedClaudeBillable(prefix) {
			p.dropped++
		}
		return
	}
	if len(line) == 0 {
		return
	}
	// recordType reads the top-level type, so whitespace in a re-serialized
	// transcript cannot hide a user or assistant record.
	switch t, ok := recordType(line); {
	case !ok && bytes.Contains(line, usageKey):
		p.dropped++
	case ok && t == "assistant" && bytes.Contains(line, usageKey):
		var rec claudeUsageRecord
		if json.Unmarshal(line, &rec) != nil {
			p.dropped++
			return
		}
		if e, ok := claudeEvent(&rec, &p.dropped); ok {
			p.events = append(p.events, e)
		}
		if a, ok := claudeActivityFromUsage(&rec); ok {
			p.activity = append(p.activity, a)
		}
	case ok && (t == "user" || t == "assistant"):
		var rec claudeActivityRecord
		if json.Unmarshal(line, &rec) != nil {
			return
		}
		if a, ok := claudeActivityFromRecord(&rec); ok {
			p.activity = append(p.activity, a)
		}
	}
}

// oversizedClaudeBillable reports whether a discarded Claude record could have
// billed. Only an assistant type bills; an unknown type fails closed.
func oversizedClaudeBillable(prefix []byte) bool {
	t, ok := recordType(prefix)
	if !ok {
		return true
	}
	return t == "assistant"
}

func sortUsageEvents(events []usageEvent) {
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
}

// claudeEvent turns one assistant record into a candidate event. Only an
// assistant record carries billed usage, and the synthetic error model is
// skipped. A record with no usable timestamp drops and increments dropped: it
// belongs to the billed timeline but cannot be placed on it. The key pairs
// message id and request id so the worktree-level dedupe can collapse streaming
// repeats and cross-file repeats to one billed turn.
func claudeEvent(rec *claudeUsageRecord, dropped *int) (usageEvent, bool) {
	msg := rec.Message
	if rec.Type != "assistant" || msg == nil || msg.Usage == nil || msg.Model == "<synthetic>" {
		return usageEvent{}, false
	}
	at, ok := parseUsageTime(rec.Timestamp)
	if !ok {
		*dropped++
		return usageEvent{}, false
	}
	tokens, tier, web := claudeTokensFrom(msg.Usage)
	return usageEvent{
		at: at, key: claudeTurnKey(msg.ID, rec.RequestID), model: msg.Model,
		tokens: tokens, tier: tier, webSearch: web,
	}, true
}

// claudeActivityFromUsage derives the turn boundary from a billed assistant
// record. A tool_use (or a streaming record with no stop reason yet) keeps the
// turn open; any terminal stop reason closes it.
func claudeActivityFromUsage(rec *claudeUsageRecord) (activityEvent, bool) {
	msg := rec.Message
	if rec.Type != "assistant" || msg == nil {
		return activityEvent{}, false
	}
	at, ok := parseUsageTime(rec.Timestamp)
	if !ok {
		return activityEvent{}, false
	}
	return activityEvent{at: at, kind: claudeStopKind(msg.StopReason)}, true
}

// claudeActivityFromRecord derives the turn boundary from a record that carries
// no billable usage: an assistant record (streaming fragment or empty turn) and
// a user record (human prompt or tool result).
func claudeActivityFromRecord(rec *claudeActivityRecord) (activityEvent, bool) {
	at, ok := parseUsageTime(rec.Timestamp)
	if !ok {
		return activityEvent{}, false
	}
	switch rec.Type {
	case "assistant":
		if rec.Message == nil {
			return activityEvent{}, false
		}
		return activityEvent{at: at, kind: claudeStopKind(rec.Message.StopReason)}, true
	case "user":
		if rec.Message == nil || rec.IsMeta {
			return activityEvent{}, false
		}
		return activityEvent{at: at, kind: claudeUserKind(rec.Message.Content)}, true
	}
	return activityEvent{}, false
}

// claudeStopKind maps Claude's stop reason to the turn state. Null is a
// streaming fragment, so it stays open; only tool_use keeps working, and every
// terminal reason closes the turn.
func claudeStopKind(stop string) activityKind {
	switch stop {
	case "", "tool_use":
		return activityWork
	default:
		return activityEnd
	}
}

// claudeUserKind distinguishes a human prompt (a string or a content block
// without tool_result) from a tool result. The content is raw so a large tool
// result is never decoded into blocks.
func claudeUserKind(content json.RawMessage) activityKind {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return activityHuman
	}
	if bytes.Contains(trimmed, []byte(`"tool_result"`)) {
		return activityWork
	}
	return activityHuman
}

// claudeTurnKey is the billed-turn identity. Both ids absent is an empty key,
// not ":": only an empty key takes the keyless pass-through, so unrelated
// id-less records never collapse into one winner.
func claudeTurnKey(msgID, requestID string) string {
	if msgID == "" && requestID == "" {
		return ""
	}
	return msgID + ":" + requestID
}

// claudeTokensFrom splits the usage into the four priced classes. A record that
// omits the cache_creation breakdown keeps the flat count as unclassified, and
// any total not covered by the breakdown is unclassified too.
func claudeTokensFrom(u *claudeUsage) (promptTokens, string, int64) {
	var t promptTokens
	t.input = derefInt(u.InputTokens)
	t.output = derefInt(u.OutputTokens)
	t.cacheRead = derefInt(u.CacheRead)
	classifyCacheWrite(&t, u)
	tier := ""
	if u.ServiceTier != nil {
		tier = *u.ServiceTier
	}
	var web int64
	if u.ServerToolUse != nil {
		web = derefInt(u.ServerToolUse.WebSearchRequests)
	}
	return t, tier, web
}

func classifyCacheWrite(t *promptTokens, u *claudeUsage) {
	if u.CacheDetail != nil {
		t.cacheWrite5m = derefInt(u.CacheDetail.Ephemeral5m)
		t.cacheWrite1h = derefInt(u.CacheDetail.Ephemeral1h)
	}
	if total := derefInt(u.CacheCreation); total > t.cacheWrite5m+t.cacheWrite1h {
		t.unclassifiedCacheWrite = total - t.cacheWrite5m - t.cacheWrite1h
	}
}
