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
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Provider  string `json:"provider"`
	ModelID   string `json:"modelId"`
	// Model and Usage are the top-level fields a `usage` record carries
	// (cache warming). Compaction and branch_summary reuse Usage without Model.
	Model   string     `json:"model"`
	Usage   *piUsage   `json:"usage"`
	Message *piMessage `json:"message"`
}

type piUsage struct {
	Input      *int64  `json:"input"`
	Output     *int64  `json:"output"`
	CacheRead  *int64  `json:"cacheRead"`
	CacheWrite *int64  `json:"cacheWrite"`
	Cost       *piCost `json:"cost"`
}

// piCost is Pi's own price breakdown. Total is authoritative; the class fields
// are the per-tier split, present in modern sessions but absent in older ones.
type piCost struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cacheRead"`
	CacheWrite *float64 `json:"cacheWrite"`
	Total      *float64 `json:"total"`
}

type piMessage struct {
	Role          string           `json:"role"`
	Provider      string           `json:"provider"`
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

// piSpawnDetails is the two shapes Pi has used for a spawn result. Modern Pi
// writes one child under Model/Route/Stats; older Pi writes one entry per child
// under Results, each with its own model and cost. Both are read so a session
// written by either version prices exactly.
type piSpawnDetails struct {
	Model   string          `json:"model"`
	Route   piSpawnRoute    `json:"route"`
	Stats   *piSpawnStats   `json:"stats"`
	Results []piSpawnResult `json:"results"`
}

// piSpawnRoute is where Pi records the provider that served a spawn child. An
// inherited child omits it, so the child then bills on the parent's provider.
type piSpawnRoute struct {
	Provider string `json:"provider"`
}

type piSpawnStats struct {
	Cost             *float64 `json:"cost"`
	InputTokens      *int64   `json:"inputTokens"`
	OutputTokens     *int64   `json:"outputTokens"`
	CacheReadTokens  *int64   `json:"cacheReadTokens"`
	CacheWriteTokens *int64   `json:"cacheWriteTokens"`
}

// piSpawnResult is one child in an older spawn result. Only the usage block is
// read; the sibling prompt and output are content-heavy and stay undecoded.
type piSpawnResult struct {
	Usage *piSpawnUsage `json:"usage"`
}

// piSpawnUsage is an older child's own billing: the model that answered, the
// cost Pi computed for it, and its token counts.
type piSpawnUsage struct {
	Model      string   `json:"model"`
	Cost       *float64 `json:"cost"`
	Input      *int64   `json:"input"`
	Output     *int64   `json:"output"`
	CacheRead  *int64   `json:"cacheRead"`
	CacheWrite *int64   `json:"cacheWrite"`
}

// piUsageEvents parses one Pi session into cost/time events, reporting how many
// records it could not read. Malformed lines are skipped, matching the session
// index's tolerant owner scan, but the count lets the caller warn instead of
// silently shrinking the total.
func piUsageEvents(r *bufio.Reader) ([]usageEvent, int, error) {
	var events []usageEvent
	dropped := 0
	spawnAt := map[string]time.Time{}
	current, provider := "", ""
	for {
		line, err := sessionRecord(r)
		if len(line) > 0 && piInteresting(line) {
			var rec piUsageRecord
			if json.Unmarshal(line, &rec) != nil {
				dropped++
			} else {
				current, provider = appendPiRecord(&events, &rec, current, provider, spawnAt, &dropped)
			}
		}
		if err != nil {
			if err == io.EOF {
				return events, dropped, nil
			}
			return events, dropped, err
		}
	}
}

// piBillsTime reports whether a record type carries billed cost/time, so it
// needs a usable timestamp; any other type is read for attribution only.
func piBillsTime(t string) bool {
	switch t {
	case "message", "usage", "compaction", "branch_summary":
		return true
	}
	return false
}

// appendPiRecord folds one record in and returns the session's current model and
// provider. Every usage-bearing record becomes an event: an assistant message,
// a top-level `usage` record (cache warming), and a compaction or branch_summary
// summary call. Only a model change moves the running attribution without
// billing.
func appendPiRecord(events *[]usageEvent, rec *piUsageRecord, current, provider string, spawnAt map[string]time.Time, dropped *int) (string, string) {
	if rec.Type == "model_change" {
		if rec.ModelID != "" {
			current = rec.ModelID
		}
		if rec.Provider != "" {
			provider = rec.Provider
		}
		return current, provider
	}
	if !piBillsTime(rec.Type) {
		return current, provider
	}
	at, ok := parseUsageTime(rec.Timestamp)
	if !ok {
		// A billed record with no usable timestamp cannot be placed on the
		// activity or cost timeline, so it drops; the count discloses that loss.
		*dropped++
		return current, provider
	}
	switch rec.Type {
	case "message":
		// The message's provider is authoritative for its event; the model
		// change only seeds the running value. Only assistant messages carry
		// the session's current provider; a toolResult with a provider field
		// belongs to a child and must not update the running provider the
		// parent inherits next.
		if rec.Message != nil && rec.Message.Provider != "" && rec.Message.Role == "assistant" {
			provider = rec.Message.Provider
		}
		recordSpawnCalls(rec.Message, at, spawnAt)
		return appendPiMessage(events, rec.Message, rec.ID, at, current, provider, spawnAt), provider
	case "usage":
		// A top-level usage record (cache warming) names its own model and
		// provider; it also seeds the running attribution for later records that
		// name neither. Pi bills it, so dropping it understates the total.
		if rec.Provider != "" {
			provider = rec.Provider
		}
		if rec.Model != "" {
			current = rec.Model
		}
		addPiEvent(events, at, rec.ID, current, provider, rec.Usage)
	case "compaction", "branch_summary":
		// A summary call reports usage but names no model, so it bills on the
		// session's running attribution.
		addPiEvent(events, at, rec.ID, current, provider, rec.Usage)
	}
	return current, provider
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

func appendPiMessage(events *[]usageEvent, msg *piMessage, id string, at time.Time, current, provider string, spawnAt map[string]time.Time) string {
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
		addPiEvent(events, at, id, model, provider, msg.Usage)
	case "toolResult":
		// A spawn result is the child agent's only record. Pi may express its
		// cost in the tool-result usage or in details.stats; the two hold the
		// same number, so read one, never both.
		if msg.ToolName == "spawn" {
			appendPiSpawn(events, at, id, current, provider, msg, spawnAt)
		} else {
			addPiEvent(events, at, id, current, provider, msg.Usage)
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

// piSpawnChild is one spawned child's billing facts.
type piSpawnChild struct {
	model    string
	provider string
	cost     *float64
}

// appendPiSpawn records one event per child Pi reports on the parent's spawn
	tokens   promptTokens
// result. The child runs in memory, so this is its only record. A failed spawn
// that reports neither a child nor a cost is a known-zero event, so it cannot
// poison the total; a non-error child with no cost stays unknown-cost. A batch
// that mixed models prices each child on its own model.
func appendPiSpawn(events *[]usageEvent, at time.Time, id string, current, provider string, msg *piMessage, spawnAt map[string]time.Time) {
	// Pair every child with its tool call so its running time counts. A missing
	// call (older session) or a result before the call degrades to a point event.
	start, paired := spawnAt[msg.ToolCallID]
	paired = paired && !start.After(at)
	for i, child := range piSpawnChildren(current, provider, msg) {
		e := usageEvent{at: at, model: child.model, provider: child.provider, cost: child.cost, tokens: child.tokens, end: at}
		if paired {
			e.at, e.end = start, at
		}
		// The record id plus child index keeps two identical children of one
		// batch distinct: the dedupe treats equal identity keys as repeats.
		e.key = piEventKeyN(e, id, fmt.Sprintf("spawn:%d", i))
		*events = append(*events, e)
	}
}

// piSpawnChildren returns every child a spawn result reports. Older Pi writes N
// children under details.results, each with its own model and cost; modern Pi
// writes a single child under details.{model,stats}. Both yield the same list,
// so the caller never branches on the schema.
func piSpawnChildren(current, provider string, msg *piMessage) []piSpawnChild {
	if d := msg.Details; d != nil && len(d.Results) > 0 {
		children := make([]piSpawnChild, 0, len(d.Results))
		for _, r := range d.Results {
			model, cost, tokens := "", (*float64)(nil), promptTokens{}
			if r.Usage != nil {
				model, cost = r.Usage.Model, r.Usage.Cost
			}
			// An older result carries no route, so the child proves nothing: it
			// keeps an empty provider, which piProven rejects, instead of
				tokens = piSpawnUsageTokens(r.Usage)
			// inheriting the parent's subscription.
			children = append(children, piSpawnChild{model: piChildModel(model, current), provider: "", cost: cost, tokens: tokens})
		}
		return children
	}
	child := msg.Details != nil && msg.Details.Model != ""
	cost := spawnCost(msg)
	if cost == nil && msg.IsError && !child {
		zero := 0.0
		cost = &zero
	}
	return []piSpawnChild{{model: piSpawnModel(current, msg), provider: piChildProvider(provider, msg), cost: cost, tokens: spawnTokens(msg)}}
}

// piSpawnModel is the child's model: Pi records it on the result, and a result
// that omits it is attributed to the parent's running model, never lost.
func piSpawnModel(current string, msg *piMessage) string {
	if msg.Details != nil {
		return piChildModel(msg.Details.Model, current)
	}
	return piChildModel("", current)
}

// piChildModel falls back from the child's own model to the parent's running
// model, and only then to "unknown".
func piChildModel(model, current string) string {
	if model != "" {
		return model
	}
	if current != "" {
		return current
	}
	return "unknown"
}

// piChildProvider is the child's billing provider. A routed child names its own
// provider on the spawn result; an inherited child names none and bills on the
// parent's. Reading the parent's provider for a routed child would misattribute
// a metered child to the parent's subscription.
func piChildProvider(parent string, msg *piMessage) string {
	if msg.Details != nil && msg.Details.Route.Provider != "" {
		return msg.Details.Route.Provider
	}
	return parent
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
func addPiEvent(events *[]usageEvent, at time.Time, id string, model, provider string, u *piUsage) {
// spawnTokens chooses one token source for a modern single child: details.stats
// first, then the tool-result usage. The two describe the same child, so adding
// both would double count it.
func spawnTokens(msg *piMessage) promptTokens {
	if msg.Details != nil {
		if tokens, ok := piSpawnStatsTokens(msg.Details.Stats); ok {
			return tokens
		}
	}
	return piTokens(msg.Usage)
}

// piSpawnStatsTokens reads the modern spawn token shape. ok is false when the
// stats block records no token class, so the caller can fall back.
func piSpawnStatsTokens(s *piSpawnStats) (promptTokens, bool) {
	if s == nil || (s.InputTokens == nil && s.OutputTokens == nil && s.CacheReadTokens == nil && s.CacheWriteTokens == nil) {
		return promptTokens{}, false
	}
	return promptTokens{
		input:        derefInt(s.InputTokens),
		output:       derefInt(s.OutputTokens),
		cacheRead:    derefInt(s.CacheReadTokens),
		cacheWrite5m: derefInt(s.CacheWriteTokens),
	}, true
}

// piSpawnUsageTokens reads the legacy per-child token shape. Each child owns its
// own usage block, so there is no parent aggregate to fall back to.
func piSpawnUsageTokens(u *piSpawnUsage) promptTokens {
	if u == nil {
		return promptTokens{}
	}
	return promptTokens{
		input:        derefInt(u.Input),
		output:       derefInt(u.Output),
		cacheRead:    derefInt(u.CacheRead),
		cacheWrite5m: derefInt(u.CacheWrite),
	}
}

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
	e := usageEvent{at: at, model: model, provider: provider, cost: cost, tokens: piTokens(u), tierCost: piTierCost(u.Cost)}
	e.key = piEventKey(e, id)
	*events = append(*events, e)
}

// piTokens reads Pi's token counts into the shared class layout. Pi keeps one
// cache-write class, so it fills the 5-minute field; the display collapses the
// two write classes anyway.
func piTokens(u *piUsage) promptTokens {
	return promptTokens{
		input:        derefInt(u.Input),
		output:       derefInt(u.Output),
	if u == nil {
		return promptTokens{}
	}
		cacheRead:    derefInt(u.CacheRead),
		cacheWrite5m: derefInt(u.CacheWrite),
	}
}

// piTierCost reads Pi's per-class price when present. Older records price only
// the total, so they yield nil and leave every tier absent.
func piTierCost(c *piCost) *tokenCosts {
	if c == nil || (c.Input == nil && c.Output == nil && c.CacheRead == nil && c.CacheWrite == nil) {
		return nil
	}
	return &tokenCosts{input: c.Input, output: c.Output, cacheRead: c.CacheRead, cacheWrite: c.CacheWrite}
}

// piEventKey is the identity of a Pi event: the entry id bound to the record
// time when the id is present, since a resumed or forked transcript repeats the
// same ids and times and must bill once. Cost is not part of an id key: a revised
// copy of one record (partial vs complete stream) shares the id and the copy
// that carries its cost wins at dedupe (see dedupeScore). A record with no id
// falls back to its content plus cost presence, which still dedupes an exact
// copy and a revised copy (the copy that carries its cost wins) while keeping a
// known zero distinct from a missing cost. Provider is not part of either identity: the
// same record can attribute a different running provider when re-parsed, and
// that must still dedupe.
func piEventKey(e usageEvent, id string) string { return piEventKeyN(e, id, "") }

// piEventKeyN is piEventKey with a per-event tag. A spawn batch passes the
// child index so two children with equal time, model and cost stay distinct.
// The id key binds the record time: a branched session copies the id and the
// timestamp verbatim (Pi createBranchedSession), so a fork still dedupes, while
// two unrelated sessions that collide on the short random id almost never share
// the exact nanosecond timestamp. Cost stays out of the id key because a revised
// copy of one record shares the id and time and the copy that carries its cost
// wins at dedupe (see dedupeScore).
// LIMIT: id-less records with equal (time, model, cost presence) share one
// fallback key; the spawn index is the only discriminator for equal children.
// TestPiIdlessSameTimestampCollisionBillsOnce pins the id-less collision;
// TestPiSameIDDifferentTimestampBillsTwice pins the time binding.
func piEventKeyN(e usageEvent, id, tag string) string {
	var key string
	if id != "" {
		key = fmt.Sprintf("%s|%d", id, e.at.UnixNano())
	} else {
		key = fmt.Sprintf("%d|%d|%s", e.at.UnixNano(), e.end.UnixNano(), e.model) + "|" + piCostTag(e)
	}
	if tag != "" {
		return key + "|" + tag
	}
	return key
}

// piCostTag encodes cost presence, not value, so the fallback matches the id
// key: a revised copy shares the identity and the copy that carries its cost
// wins at dedupe (see dedupeScore). Presence still separates a known zero from a missing cost: nil is unknown,
// not $0.
func piCostTag(e usageEvent) string {
	if e.cost == nil {
		return "?"
	}
	return "1"
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
