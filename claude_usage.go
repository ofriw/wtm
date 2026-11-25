package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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
	Model      string          `json:"model"`
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

// UnmarshalJSON reads each figure on its own. A typed decode of the whole block
// fails on the first number this build cannot hold, and with it loses every
// readable count of the turn (see rawFields).
func (u *claudeUsage) UnmarshalJSON(b []byte) error {
	f, err := rawObject(b)
	if err != nil {
		return err
	}
	u.InputTokens, u.OutputTokens = f.count("input_tokens"), f.count("output_tokens")
	u.CacheRead, u.CacheCreation = f.count("cache_read_input_tokens"), f.count("cache_creation_input_tokens")
	u.ServiceTier = f.text("service_tier")
	if u.CacheDetail, err = decodeObject[claudeCacheCreation](f, "cache_creation"); err != nil {
		return err
	}
	u.ServerToolUse, err = decodeObject[claudeServerToolUse](f, "server_tool_use")
	return err
}

type claudeCacheCreation struct {
	Ephemeral5m *int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h *int64 `json:"ephemeral_1h_input_tokens"`
}

// UnmarshalJSON reads each lifetime figure on its own (see claudeUsage).
func (c *claudeCacheCreation) UnmarshalJSON(b []byte) error {
	f, err := rawObject(b)
	if err != nil {
		return err
	}
	c.Ephemeral5m, c.Ephemeral1h = f.count("ephemeral_5m_input_tokens"), f.count("ephemeral_1h_input_tokens")
	return nil
}

type claudeServerToolUse struct {
	WebSearchRequests *int64 `json:"web_search_requests"`
}

// UnmarshalJSON reads the search count on its own (see claudeUsage).
func (s *claudeServerToolUse) UnmarshalJSON(b []byte) error {
	f, err := rawObject(b)
	if err != nil {
		return err
	}
	s.WebSearchRequests = f.count("web_search_requests")
	return nil
}

// claudeUsageEvents parses one Claude session into cost events and causal turn
// activity. Unreadable records are counted so the caller can warn.
func claudeUsageEvents(r *bufio.Reader) (sessionParse, error) {
	var out sessionParse
	for {
		line, oversized, prefix, rerr := sessionRecord(r)
		err := sessionRecordErr(line, oversized, rerr)
		out.consumeClaudeLine(line, oversized, prefix)
		if err == io.EOF {
			out.sortClaude()
			return out, nil
		}
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				// A killed writer cut the final record mid-line. The completed
				// records survive; the tail is a record-level loss the caller
				// taints, instead of dropping the whole session.
				return out, err
			}
			// One error policy: any other read failure drops the session and
			// taints the total, so a partial result must never read as complete.
			return sessionParse{}, err
		}
	}
}

func (p *sessionParse) sortClaude() {
	sortUsageEvents(p.events)
	sortActivity(p.activity)
}

// consumeClaudeLine folds one record: a billed assistant drives cost and the
// turn state, a usage-less user/assistant record drives the turn alone, a
// malformed record that visibly carried usage taints the total, and a readable
// record whose type no branch consumes but that still carries usage fails
// closed, naming the type.
func (p *sessionParse) consumeClaudeLine(line []byte, oversized bool, prefix []byte) {
	if oversized {
		p.consumeOversizedClaude(prefix)
		return
	}
	if len(line) == 0 {
		return
	}
	typ, typOK, hasUsage, malformed := scanRecord(line)
	switch {
	case typOK && typ == "assistant" && hasUsage:
		p.consumeClaudeBilled(line)
	case malformed && (bytes.Contains(line, usageKey) || bytes.Contains(line, piEscapedUsageKey)):
		p.dropped++
	case hasUsage:
		p.dropUnsupported(typ, typOK)
	case typOK && (typ == "user" || typ == "assistant"):
		p.consumeClaudeActivity(line)
	}
}

// dropUnsupported counts a readable usage-bearing record whose type no branch
// consumes, naming the type so the loss discloses what was unsupported.
func (p *sessionParse) dropUnsupported(t string, ok bool) {
	p.dropped++
	if ok {
		p.markUnsupported(t)
	}
}

func (p *sessionParse) markUnsupported(t string) {
	if p.unsupported == nil {
		p.unsupported = map[string]int{}
	}
	p.unsupported[t]++
}

// consumeOversizedClaude counts a discarded record only when it could have
// billed; a truncated prefix cannot prove usage absence, so an assistant bills,
// and an unknown usage-bearing type is named for the disclosure.
func (p *sessionParse) consumeOversizedClaude(prefix []byte) {
	typ, typOK, hasUsage, _ := scanRecord(prefix)
	switch {
	case !typOK || typ == "assistant":
		p.dropped++
	case hasUsage:
		p.dropped++
		p.markUnsupported(typ)
	}
}

// consumeClaudeBilled folds one assistant record carrying usage: the priced
// event and the turn activity share the record identity.
func (p *sessionParse) consumeClaudeBilled(line []byte) {
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
}

// consumeClaudeActivity folds one usage-less user or assistant record into the
// turn state.
func (p *sessionParse) consumeClaudeActivity(line []byte) {
	var rec claudeActivityRecord
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	if a, ok := claudeActivityFromRecord(&rec); ok {
		p.activity = append(p.activity, a)
	}
}

func sortUsageEvents(events []usageEvent) {
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
}

// claudeSyntheticModel is the error record's pseudo-model. It bills nothing and
// names no answering model, so it can never own a turn's opening.
const claudeSyntheticModel = "<synthetic>"

// claudeModelName maps an empty assistant model to the shared unknown label, so
// a truncated or third-party write never renders a blank model row or JSON key.
func claudeModelName(model string) string {
	if model == "" {
		return unknownValue
	}
	return model
}

// claudeAnswer is the model an activity record proves; the synthetic error
// record names none.
func claudeAnswer(model string) string {
	if model == claudeSyntheticModel {
		return ""
	}
	return model
}

// claudeEvent turns one assistant record into a candidate event. Only an
// assistant record carries billed usage, and the synthetic error model is
// skipped. A record with no usable timestamp drops and increments dropped: it
// belongs to the billed timeline but cannot be placed on it. The key pairs
// message id and request id so the worktree-level dedupe can collapse streaming
// repeats and cross-file repeats to one billed turn.
func claudeEvent(rec *claudeUsageRecord, dropped *int) (usageEvent, bool) {
	msg := rec.Message
	if rec.Type != "assistant" || msg == nil || msg.Usage == nil || msg.Model == claudeSyntheticModel {
		return usageEvent{}, false
	}
	at, ok := transcriptTimestamp(rec.Timestamp)
	if !ok {
		*dropped++
		return usageEvent{}, false
	}
	tokens, tier, web := claudeTokensFrom(msg.Usage)
	return usageEvent{
		at: at, key: claudeTurnKey(msg.ID, rec.RequestID), model: claudeModelName(msg.Model),
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
	at, ok := transcriptTimestamp(rec.Timestamp)
	if !ok {
		return activityEvent{}, false
	}
	return activityEvent{at: at, kind: claudeStopKind(msg.StopReason), answer: claudeAnswer(msg.Model)}, true
}

// claudeActivityFromRecord derives the turn boundary from a record that carries
// no billable usage: an assistant record (streaming fragment or empty turn) and
// a user record (human prompt or tool result).
func claudeActivityFromRecord(rec *claudeActivityRecord) (activityEvent, bool) {
	at, ok := transcriptTimestamp(rec.Timestamp)
	if !ok {
		return activityEvent{}, false
	}
	switch rec.Type {
	case "assistant":
		if rec.Message == nil {
			return activityEvent{}, false
		}
		return activityEvent{at: at, kind: claudeStopKind(rec.Message.StopReason), answer: claudeAnswer(rec.Message.Model)}, true
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

// claudeUserKind distinguishes a human prompt from a tool result by block type.
// A text value that happens to spell tool_result must not turn a human prompt
// into work, so the raw bytes are scanned structurally: the decoder reads only
// each block's type and skips content-heavy fields without a second copy.
func claudeUserKind(content json.RawMessage) activityKind {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return activityHuman
	}
	if claudeContentHasToolResult(trimmed) {
		return activityWork
	}
	return activityHuman
}

// claudeContentHasToolResult reports whether a content block array carries a
// tool_result block. Malformed JSON stays human: activity never prices, and a
// prompt should close the previous turn rather than keep it open.
func claudeContentHasToolResult(content []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(content))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('[') {
		return false
	}
	for dec.More() {
		var block struct {
			Type string `json:"type"`
		}
		if err := dec.Decode(&block); err != nil {
			return false
		}
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
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
	total := derefInt(u.CacheCreation)
	if invalidCount(total) != "" {
		// An invalid flat total voids the whole tier: its derived classes cannot
		// be trusted to sum to a real recorded value. Keeping the bad figure
		// visible makes sanitizeTokens reject the tier instead of pricing an
		// exact-looking but fabricated split.
		t.unclassifiedCacheWrite = total
		return
	}
	if u.CacheDetail != nil {
		t.cacheWrite5m = derefInt(u.CacheDetail.Ephemeral5m)
		t.cacheWrite1h = derefInt(u.CacheDetail.Ephemeral1h)
	}
	t.classifyWriteTotal(total, u.CacheCreation != nil)
}

// classifyWriteTotal folds the flat cache-write total into the lifetime split.
// A remainder the breakdown does not explain stays unclassified; a breakdown
// that outruns a stated total contradicts it, so no write figure of that record
// can be trusted and the tier is flagged for sanitizeTokens to reject and name.
// An absent total states nothing, so the split stands on its own.
func (t *promptTokens) classifyWriteTotal(total int64, stated bool) {
	switch {
	case stated && total < t.cacheWrite5m+t.cacheWrite1h:
		t.conflicts = t.conflicts.with(tierCacheWrite)
	case total > t.cacheWrite5m+t.cacheWrite1h:
		t.unclassifiedCacheWrite = total - t.cacheWrite5m - t.cacheWrite1h
	}
}
