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
// dedupe (dedupeKeyedEvents) keeps the largest record, so a turn counts once
// even when a resumed or forked session file repeats it.

type claudeUsageRecord struct {
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	RequestID string         `json:"requestId"`
	Message   *claudeMessage `json:"message"`
}

type claudeMessage struct {
	ID    string       `json:"id"`
	Model string       `json:"model"`
	Usage *claudeUsage `json:"usage"`
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

// claudeUsageEvents parses one Claude session into candidate events, one per
// assistant record, sorted by time. Turn dedupe happens at the worktree level.
func claudeUsageEvents(r *bufio.Reader) ([]usageEvent, error) {
	var events []usageEvent
	for {
		line, err := sessionRecord(r)
		if len(line) > 0 && bytes.Contains(line, usageKey) {
			var rec claudeUsageRecord
			if json.Unmarshal(line, &rec) == nil {
				if e, ok := claudeEvent(&rec); ok {
					events = append(events, e)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
				return events, nil
			}
			return nil, err
		}
	}
}

// claudeEvent turns one assistant record into a candidate event. Only an
// assistant record carries billed usage, and the synthetic error model is
// skipped. The key pairs message id and request id so the
// worktree-level dedupe can collapse streaming repeats and cross-file repeats
// to one billed turn.
func claudeEvent(rec *claudeUsageRecord) (usageEvent, bool) {
	msg := rec.Message
	if rec.Type != "assistant" || msg == nil || msg.Usage == nil || msg.Model == "<synthetic>" {
		return usageEvent{}, false
	}
	at, ok := parseUsageTime(rec.Timestamp)
	if !ok {
		return usageEvent{}, false
	}
	tokens, tier, web := claudeTokensFrom(msg.Usage)
	return usageEvent{
		at: at, key: msg.ID + ":" + rec.RequestID, model: msg.Model,
		tokens: tokens, tier: tier, webSearch: web,
	}, true
}

// claudeTokensFrom splits the usage into the four priced classes. A record that
// omits the cache_creation breakdown keeps the flat count as unclassified, and
// any total not covered by the breakdown is unclassified too.
func claudeTokensFrom(u *claudeUsage) (claudeTokens, string, int64) {
	var t claudeTokens
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

func classifyCacheWrite(t *claudeTokens, u *claudeUsage) {
	if u.CacheDetail != nil {
		t.cacheWrite5m = derefInt(u.CacheDetail.Ephemeral5m)
		t.cacheWrite1h = derefInt(u.CacheDetail.Ephemeral1h)
	}
	if total := derefInt(u.CacheCreation); total > t.cacheWrite5m+t.cacheWrite1h {
		t.unclassifiedCacheWrite = total - t.cacheWrite5m - t.cacheWrite1h
	}
}

func (t claudeTokens) total() int64 {
	return t.input + t.output + t.cacheRead + t.cacheWrite5m + t.cacheWrite1h + t.unclassifiedCacheWrite
}

func derefInt(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
