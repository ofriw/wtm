package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// pi_usage.go — the Pi.dev usage parser. Pi prices its own records (cost.total),
// so this parser only reads costs and timestamps; the catalog is never used for
// Pi. The event set mirrors Pi's own session accounting plus the spawn child,
// which lives only in the parent's tool result (children run in memory).

type piUsageRecord struct {
	Type      string     `json:"type"`
	Timestamp string     `json:"timestamp"`
	ModelID   string     `json:"modelId"`
	Message   *piMessage `json:"message"`
}

type piUsage struct {
	Cost *piCost `json:"cost"`
}

type piCost struct {
	Total *float64 `json:"total"`
}

type piMessage struct {
	Role          string           `json:"role"`
	Model         string           `json:"model"`
	ResponseModel string           `json:"responseModel"`
	ToolName      string           `json:"toolName"`
	ToolCallID    string           `json:"toolCallId"`
	IsError       bool             `json:"isError"`
	Content       []piContentBlock `json:"content"`
	Usage         *piUsage         `json:"usage"`
	Details       *piSpawnDetails  `json:"details"`
}

// piContentBlock is the subset of an assistant content block the parser needs:
// a spawn toolCall names the child, and its id pairs the call with its result so
// the child's running time can be recovered.
type piContentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type piSpawnDetails struct {
	Model string        `json:"model"`
	Stats *piSpawnStats `json:"stats"`
}

type piSpawnStats struct {
	Cost *float64 `json:"cost"`
}

// piUsageEvents parses one Pi session into cost/time events. Malformed lines are
// skipped, matching the session index's tolerant owner scan.
func piUsageEvents(r *bufio.Reader) ([]usageEvent, error) {
	var events []usageEvent
	spawnAt := map[string]time.Time{}
	current := ""
	for {
		line, err := sessionRecord(r)
		if len(line) > 0 && piInteresting(line) {
			var rec piUsageRecord
			if json.Unmarshal(line, &rec) == nil {
				current = appendPiRecord(&events, &rec, current, spawnAt)
			}
		}
		if err != nil {
			if err == io.EOF {
				return events, nil
			}
			return events, err
		}
	}
}

// appendPiRecord folds one record in and returns the session's current model.
// Only a message carries usage; a model change only moves the current model.
func appendPiRecord(events *[]usageEvent, rec *piUsageRecord, current string, spawnAt map[string]time.Time) string {
	if rec.Type == "model_change" {
		if rec.ModelID != "" {
			return rec.ModelID
		}
		return current
	}
	if rec.Type != "message" {
		return current
	}
	at, ok := parseUsageTime(rec.Timestamp)
	if !ok {
		return current
	}
	recordSpawnCalls(rec.Message, at, spawnAt)
	return appendPiMessage(events, rec.Message, at, current, spawnAt)
}

// recordSpawnCalls remembers when each spawn tool call started. The call and its
// result share an id, so the result can recover the child's running time.
func recordSpawnCalls(msg *piMessage, at time.Time, spawnAt map[string]time.Time) {
	if msg == nil {
		return
	}
	for _, b := range msg.Content {
		if b.Type == "toolCall" && b.Name == "spawn" && b.ID != "" {
			spawnAt[b.ID] = at
		}
	}
}

func appendPiMessage(events *[]usageEvent, msg *piMessage, at time.Time, current string, spawnAt map[string]time.Time) string {
	if msg == nil {
		return current
	}
	switch msg.Role {
	case "assistant":
		model := piMessageModel(msg)
		if model == "" {
			// A usage record can omit its model; attribute it to the session's
			// current model instead of losing the attribution to "unknown".
			model = current
		}
		if model != "" {
			current = model
		}
		addPiEvent(events, at, model, msg.Usage)
	case "toolResult":
		// A spawn result is the child agent's only record. Pi may express its
		// cost in the tool-result usage or in details.stats; the two hold the
		// same number, so read one, never both.
		if msg.ToolName == "spawn" {
			appendPiSpawn(events, at, current, msg, spawnAt)
		} else {
			addPiEvent(events, at, current, msg.Usage)
		}
	}
	return current
}

// piMessageModel prefers the physical model that answered, so a virtual model
// routes to the real one.
func piMessageModel(msg *piMessage) string {
	if msg.ResponseModel != "" {
		return msg.ResponseModel
	}
	return msg.Model
}

// appendPiSpawn records the child-agent cost Pi reports on the parent's spawn
// result. The child runs in memory, so this is the only record of it. A failed
// spawn is still an event: it carries a known-zero cost when it reports neither
// a child nor a cost, so it cannot poison the total. A non-error child with no
// cost stays an unknown-cost event (design open item).
func appendPiSpawn(events *[]usageEvent, at time.Time, current string, msg *piMessage, spawnAt map[string]time.Time) {
	child := msg.Details != nil && msg.Details.Model != ""
	model := ""
	if msg.Details != nil {
		model = msg.Details.Model
	}
	if model == "" {
		model = current
	}
	if model == "" {
		model = "unknown"
	}
	cost := spawnCost(msg)
	if cost == nil && msg.IsError && !child {
		zero := 0.0
		cost = &zero
	}
	e := usageEvent{at: at, model: model, cost: cost, end: at}
	// Pair the result with its tool call so the child's running time counts.
	// A missing call (older session) degrades to a point event.
	if start, ok := spawnAt[msg.ToolCallID]; ok && !start.After(at) {
		e.at, e.end = start, at
	}
	e.key = piEventKey(e)
	*events = append(*events, e)
}

// spawnCost reads the child cost from details.stats, the documented location,
// and falls back to the tool-result usage when a child omits stats.
func spawnCost(msg *piMessage) *float64 {
	if msg.Details != nil && msg.Details.Stats != nil && msg.Details.Stats.Cost != nil {
		return msg.Details.Stats.Cost
	}
	if msg.Usage != nil && msg.Usage.Cost != nil {
		return msg.Usage.Cost.Total
	}
	return nil
}

// addPiEvent records one Pi usage; a missing cost.total is a priced-unknown
// event that still counts toward active time.
func addPiEvent(events *[]usageEvent, at time.Time, model string, u *piUsage) {
	if u == nil {
		return
	}
	if model == "" {
		model = "unknown"
	}
	var cost *float64
	if u.Cost != nil {
		cost = u.Cost.Total
	}
	e := usageEvent{at: at, model: model, cost: cost}
	e.key = piEventKey(e)
	*events = append(*events, e)
}

// piEventKey is the content identity of a Pi event. A resumed or forked
// transcript repeats the same records across session files, so identical
// content bills once at the worktree-level dedupe. Costs compare by value:
// pointer identity would keep every repeat.
func piEventKey(e usageEvent) string {
	var cost float64
	if e.cost != nil {
		cost = *e.cost
	}
	return fmt.Sprintf("%d|%d|%s|%v", e.at.UnixNano(), e.end.UnixNano(), e.model, cost)
}

// piInteresting is the cheap pre-filter: only usage records, a spawn child's
// stats and details, a model change, and an error result can affect cost or
// model attribution, so the decoder never touches content-heavy lines. isError
// matters on its own: a failed spawn that reports neither a child nor a cost is
// still an event (a known zero), and such a record may carry no other key.
var (
	piStatsKey       = []byte("\"stats\"")
	piModelChangeKey = []byte("\"model_change\"")
	piDetailsKey     = []byte("\"details\"")
	piToolCallKey    = []byte("\"toolCall\"")
	piIsErrorKey     = []byte("\"isError\"")
)

func piInteresting(line []byte) bool {
	return bytes.Contains(line, usageKey) || bytes.Contains(line, piStatsKey) ||
		bytes.Contains(line, piModelChangeKey) || bytes.Contains(line, piDetailsKey) ||
		bytes.Contains(line, piToolCallKey) || bytes.Contains(line, piIsErrorKey)
}

// parseUsageTime parses an entry's ISO timestamp.
func parseUsageTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
