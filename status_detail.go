package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// status_detail.go — the status detail block: its schema, width fit, summary
// and per-model token rows, and the legend it renders.

// detailFit is the breakdown block's width fit and its legend needs. One fit
// serves the renderer and the legend pass, so the two cannot disagree about
// what the block renders. It carries the fitted widths, the summary drop
// decision, and the token-group tokens flag, so no caller re-fits to answer a
// kept-column or legend question.
type detailFit struct {
	tokenGroupFit
	reasons estimateReasons
	list    bool
	partial bool
	tokens  bool       // token group survives and has tokens (legend)
	sum     [][]string // summary rows after hiding elided groups
	hidden  [][]string // hidden summary label-groups, disclosed by the renderer
}

// fitDetailBlock fits the block at the budget silently; the caller discloses a
// dropped token group (the renderer) or stays silent (the legend pass). The
// reasons fold in the summary pair's caveats (unless the summary hid the cost
// row) and the model rows' caveats (unless the fit dropped COST). The fit
// mirrors the renderer exactly — the token group drops whole and frees width —
// so a column the block actually renders is never judged dropped here.
func fitDetailBlock(cur worktree, now time.Time, budget int) detailFit {
	var f detailFit
	summary := detailSummaryRows(cur, now)
	f.hidden = summaryHiddenGroups(summary, budget)
	f.sum = dropHiddenSummaryRows(summary, f.hidden)
	if !groupHidden(f.hidden, detailCostLabel) {
		f.reasons = f.reasons.merge(cur.cost().reasons())
	}
	if cur.Usage == nil {
		return f
	}
	cols, rows := detailSchema(cur.Usage)
	f.tokenGroupFit = fitTokenGroupPlan(cols, rows, budget)
	if keptAt(f.cols, f.widths, colCost.header) {
		f.reasons = f.reasons.merge(modelsEstimated(cur.Usage))
	}
	f.list = keptAt(f.cols, f.widths, colList.header)
	f.tokens = tokenGroupKept(f.cols) && detailHasTokens(cur.Usage)
	f.partial = detailPartial(cur, f)
	return f
}

// keptAt reports whether the named column survived the fit, straight from the
// stored widths — no re-fit.
func keptAt(cols []worktreeColumn, widths []int, header string) bool {
	for i, c := range cols {
		if c.header == header {
			return widths[i] != 0
		}
	}
	return false
}

// groupHidden reports whether any hidden summary group carries label.
func groupHidden(groups [][]string, label string) bool {
	for _, g := range groups {
		if slices.Contains(g, label) {
			return true
		}
	}
	return false
}

// detailPartial reports whether the fitted detail block renders a partial
// marker: the summary ACTIVE value, a surviving ratio row, a model time, or a
// surviving token cell. A total loss renders only "unknown", so it needs no
// dangling "~" legend. All decisions come from the single fit — hidden summary
// groups and the fitted widths — so nothing is re-derived.
func detailPartial(cur worktree, f detailFit) bool {
	if cur.Usage == nil || !cur.Usage.partial {
		return false
	}
	if detailSummaryPartial(cur, f.hidden) {
		return true
	}
	return detailModelPartial(cur.Usage, f.cols, f.widths)
}

// detailSummaryPartial reports a partial marker in the summary: the ACTIVE value
// or a ratio row that survives the width fit (via the stored hidden groups).
func detailSummaryPartial(cur worktree, hidden [][]string) bool {
	if _, marked := activeCellValue(cur); marked {
		return true
	}
	return cur.Usage.tokens().ratiosDefined() && !groupHidden(hidden, detailRatioMeta[0].label)
}

// detailModelPartial reports a partial model row whose TIME or token cell
// survives, from the fitted widths.
func detailModelPartial(u *worktreeUsage, cols []worktreeColumn, widths []int) bool {
	timeKept := keptAt(cols, widths, colTime.header)
	tokensKept := tokenGroupKept(cols)
	for _, m := range u.mergedModels() {
		if m.partial && ((timeKept && m.active() > 0) || (tokensKept && m.tokensKnown())) {
			return true
		}
	}
	return false
}

// modelsEstimated reports which legend reasons any model row rests on. Only the
// model total counts: per-tier costs are no longer rendered, and a tier's
// assumption propagates into the total, so the total's own reasons already
// cover every estimate the visible rows rest on.
func modelsEstimated(u *worktreeUsage) estimateReasons {
	var r estimateReasons
	if u == nil {
		return r
	}
	for _, h := range u.harnesses {
		for _, m := range h.models {
			r = r.merge(m.cost.reasons())
		}
	}
	return r
}

// detailColumns is the detail block's static table schema: one MODEL row per
// model, TIME, COST, then the token group (total input, the token classes, then
// the derived miss/hit% metrics). A function, like columnRegistry, so the
// headers derive from the metadata lists with no package-init ordering
// dependency. It shares the status column registry (see allColumns), so the
// block obeys the same width policy as the table above: LIST drops whole first,
// MODEL elides to its floor, then the token group drops whole. That order keeps
// the breakdown visible when the terminal has room for it. TIME and COST are
// unelidable, so each renders its whole value or drops whole; COST has the
// highest rank, so it outlives the columns that name the rows. LIST is inserted
// beside COST by detailSchema when a model carries a list price.
func detailColumns() []worktreeColumn {
	cols := []worktreeColumn{colModel, colTime, colCost}
	return append(cols, tokenColumns()...)
}

// detailSchema is the breakdown's printed schema and its matching rows. Schema
// and rows are built together, so a cell can never land on the wrong column.
func detailSchema(u *worktreeUsage) ([]worktreeColumn, [][]string) {
	showList := detailShowsList(u)
	return withListColumn(detailColumns(), showList), detailRows(u, showList)
}

// detailShowsList reports whether any model row carries a list price, so the
// breakdown grows its LIST column only when it has a value to show.
func detailShowsList(u *worktreeUsage) bool {
	if u == nil {
		return false
	}
	for _, m := range u.mergedModels() {
		if m.cost.showsList() {
			return true
		}
	}
	return false
}

// printStatusDetail renders the current worktree's usage under the table. The
// block is omitted when no harness in the repo has events; when the current
// worktree alone is empty, the block says so instead of showing empty rows.
func printStatusDetail(wts []worktree, current string, now time.Time, colorMode string, budget int, seen map[string]bool) {
	if !repoHasUsage(wts) {
		return
	}
	cur := findWorktree(wts, current)
	if cur == nil {
		return
	}
	fmt.Println()
	f := fitDetailBlock(*cur, now, budget)
	printDetailSummary(f, colorMode, budget, seen)
	if cur.Usage == nil {
		fmt.Println("no agent usage recorded")
		return
	}
	fmt.Println()
	printDetailTable(f, colorMode, budget, seen)
}

// printDetailSummary prints the identity and measure rows above the breakdown,
// from the single fit: the hidden summary groups are disclosed once here, then
// the surviving rows render.
func printDetailSummary(f detailFit, colorMode string, budget int, seen map[string]bool) {
	for _, group := range f.hidden {
		warnHiddenColumns(group, seen)
	}
	printKeyValues(f.sum, detailTitleRows, colorMode, budget)
}

// printDetailTable prints the fitted model rows and their legend. An activity-
// only worktree has no model rows: a header-only grid and the width warning for
// token columns it cannot render would both disclose nothing real, so only the
// applicable legend prints. The grid renders from the fit's stored widths via
// the shared pipeline, so it is byte-identical to the table path.
func printDetailTable(f detailFit, colorMode string, budget int, seen map[string]bool) {
	if len(f.rows) == 0 {
		printDetailLegend(f.reasons, f.list, f.partial, false, colorMode, budget)
		return
	}
	if f.dropped {
		warnHiddenColumns(tokenColumnNames(), seen)
	}
	headers, rows, widths, aligns := fittedGrid(f.cols, f.rows, f.widths, seen)
	emitGrid(headers, rows, widths, aligns, colorMode, statusCellRenderer(columnRegistry()))
	printDetailLegend(f.reasons, f.list, f.partial, f.tokens, colorMode, budget)
}

// tokenHeaderSet is the set of detail headers owned by the token group.
func tokenHeaderSet() map[string]bool {
	set := make(map[string]bool, len(detailTokenMeta))
	for _, c := range tokenColumns() {
		set[c.header] = true
	}
	return set
}

// tokenColumnNames lists the token-group headers in display order, for
// disclosure.
func tokenColumnNames() []string {
	cols := tokenColumns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.header
	}
	return names
}

// tokenGroupFit is the token group's fit verdict: surviving schema and rows,
// fitted widths, and whether the group dropped whole.
type tokenGroupFit struct {
	cols    []worktreeColumn
	rows    [][]string
	widths  []int
	dropped bool
}

// fitTokenGroupPlan is the token group's pure fit decision: the schema and
// rows that survive (the group drops whole, so the grid stays index-aligned),
// the fitted widths of the surviving schema, and whether anything was dropped.
// It is silent, so the renderer discloses once and the legend pass can ask the
// same question without repeating the disclosure. When the group drops, the
// widths are re-fit on the surviving schema: dropping frees width, so a
// survivor may grow.
func fitTokenGroupPlan(cols []worktreeColumn, rows [][]string, budget int) tokenGroupFit {
	widths := columnWidths(columnHeaders(cols), rows, budget)
	token := tokenHeaderSet()
	drop := false
	for i, c := range cols {
		drop = drop || (token[c.header] && widths[i] == 0)
	}
	if !drop {
		return tokenGroupFit{cols: cols, rows: rows, widths: widths}
	}
	kept, out := withoutTokenGroup(cols, rows)
	return tokenGroupFit{cols: kept, rows: out, widths: columnWidths(columnHeaders(kept), out, budget), dropped: true}
}

func withoutTokenGroup(cols []worktreeColumn, rows [][]string) ([]worktreeColumn, [][]string) {
	token := tokenHeaderSet()
	keep := make([]int, 0, len(cols))
	kept := make([]worktreeColumn, 0, len(cols))
	for i, c := range cols {
		if !token[c.header] {
			keep = append(keep, i)
			kept = append(kept, c)
		}
	}
	out := make([][]string, len(rows))
	for r, row := range rows {
		out[r] = selectStrings(row, keep)
	}
	return kept, out
}

// detailTitleRows is how many leading detail-block rows are section titles;
// printKeyValues bolds their labels.
const detailTitleRows = 1

// detailCostLabel names the summary cost row; the builder and the narrow-
// terminal drop share it, so they cannot disagree on which row is the cost.
const detailCostLabel = "cost"

// detailSummaryRows is the block's identity row plus its age/active/cost
// measures and, when the worktree recorded output, its input:output ratios. The
// identity value keeps the full path: identifying the current worktree is the
// block's purpose, and the table above carries the elided form.
func detailSummaryRows(cur worktree, now time.Time) [][]string {
	rows := [][]string{
		{"current worktree", displayPath(cur.Path)},
		{"age", detailAge(cur, now)},
		{"active time", detailActive(cur)},
		{detailCostLabel, detailCost(cur)},
	}
	if cur.Usage != nil {
		rows = append(rows, tokenRatioRows(cur.Usage.tokens(), cur.Usage.partial)...)
	}
	return rows
}

// detailRatioMeta is the summary's input:output ratio group in display order:
// the label that names each share and the numerator it divides by. All three
// divide the same denominator (the recorded output), so they are comparable and
// add up (total = cached + uncached). The row builder and the narrow-terminal
// drop read this one list, so they cannot drift.
var detailRatioMeta = []struct {
	label   string
	jsonKey string
	num     func(promptTokens) int64
}{
	{"total in:out", "total", promptTokens.context},
	{"cached in:out", "cached", func(t promptTokens) int64 { return t.cacheRead }},
	{"uncached in:out", "uncached", promptTokens.miss},
}

// tokenRatioRows renders the summary's ratio group. A whole total with no output
// shows nothing: a ratio without a denominator is undefined. A partial total
// shows unknown rows instead, because lost records may hold the output.
func tokenRatioRows(t promptTokens, partial bool) [][]string {
	if !t.ratiosDefined() {
		if !partial {
			return nil
		}
		// A lost record may carry the missing output, so the ratio is unknown
		// rather than absent.
		rows := make([][]string, 0, len(detailRatioMeta))
		for _, d := range detailRatioMeta {
			rows = append(rows, []string{d.label, unknownValue})
		}
		return rows
	}
	rows := make([][]string, 0, len(detailRatioMeta))
	for _, d := range detailRatioMeta {
		v, _ := t.outputRatio(d.num(t))
		rows = append(rows, []string{d.label, partialValue(formatRatio(v), partial)})
	}
	return rows
}

func repoHasUsage(wts []worktree) bool {
	for _, w := range wts {
		if w.Usage != nil && w.Usage.hasEvents() {
			return true
		}
	}
	return false
}

func findWorktree(wts []worktree, path string) *worktree {
	for i := range wts {
		if wts[i].Path == path {
			return &wts[i]
		}
	}
	return nil
}

// dropHiddenSummaryRows drops the summary rows the fitted value column cannot
// hold whole: the cost row alone, and the ratio group as a unit. It is silent;
// the renderer discloses the dropped groups once. A middle-cut number reads as
// a wrong one, and a partial set of shares reads as arbitrary.
func dropHiddenSummaryRows(rows [][]string, groups [][]string) [][]string {
	var hidden []string
	for _, group := range groups {
		hidden = append(hidden, group...)
	}
	return slices.DeleteFunc(slices.Clone(rows), func(row []string) bool {
		return slices.Contains(hidden, row[0])
	})
}

// summaryHiddenGroups is the shared, silent decision for rendering and legends.
func summaryHiddenGroups(rows [][]string, budget int) [][]string {
	fitted := summaryFittedWidth(rows, budget)
	var hidden [][]string
	for _, group := range [][]string{{detailCostLabel}, ratioLabels()} {
		for _, label := range group {
			if summaryValueElided(rows, label, fitted) {
				hidden = append(hidden, group)
				break
			}
		}
	}
	return hidden
}

// ratioLabels lists the ratio group's labels in display order, for the narrow-
// terminal disclosure. The group drops as a unit, so the warning names every
// label even when one member is what overflowed.
func ratioLabels() []string {
	labels := make([]string, len(detailRatioMeta))
	for i, d := range detailRatioMeta {
		labels[i] = d.label
	}
	return labels
}

// summaryFittedWidth is the value-column width the summary block renders at, so
// a caller can tell whether a row's value survives whole. Callers pass the
// block's own rows, which always have a first row.
func summaryFittedWidth(rows [][]string, budget int) int {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0
	}
	_, fitted := sizedKeyValues(rows, budget)
	return fitted[1]
}

// summaryValueElided reports whether a named summary row's value is wider than
// the fitted value column, so its rendered cell would middle-cut.
func summaryValueElided(rows [][]string, label string, fitted int) bool {
	for _, r := range rows {
		if r[0] == label {
			return dispWidth(r[1]) > fitted
		}
	}
	return false
}

func detailAge(w worktree, now time.Time) string { return cellAge(w, columnContext{now: now}) }

func detailActive(w worktree) string { return cellActive(w, columnContext{}) }

// detailCost is the summary block's cost value: the billed/list pair, because a
// headerless key/value line has no column header to align, so one compact value
// reads better than two rows. The breakdown renders the same pair as its COST
// and LIST columns; both derive from costPair.
func detailCost(w worktree) string {
	if w.Usage == nil {
		return dashValue
	}
	return w.Usage.cost().cell()
}

// detailMetric is one column of the token group: the glyph that tags it, the
// label that names it, and the cell that renders one model's value. Tiers and
// derived metrics share the shape, so the header and the values under it read
// one list and cannot drift.
type detailMetric struct {
	glyph string
	label string
	cell  func(*modelUsage) string
}

// detailTokenMeta is the token group in display order. "in" is the TOTAL prompt
// size (uncached input + cache read + cache write), not the uncached slice, so it
// can never read smaller than "hit"; the derived miss and hit% close the group.
//
// Glyph invariant (guarded by TestStatusDetailTokenGlyphs): one display cell, text
// presentation, no U+FE0F, no color. A 2-cell or color glyph (emoji) overflows
// its box and clips the neighbouring label, and a fallback-font dingbat renders
// thin beside the bold header. So hit/write use the hooked cache arrows and
// miss/hit% use the universally covered MULTIPLICATION SIGN and BLACK CIRCLE.
var detailTokenMeta = []detailMetric{
	{"↑", "in", totalInputCell},
	{"↓", "out", tierCount(tierOutput)},
	{"↩", "hit", tierCount(tierCacheRead)},
	{"↪", "write", tierCount(tierCacheWrite)},
	{"×", "miss", missCount},
	{"●", "hit%", hitRateCell},
}

// tokenLegendTitle headings the token clauses, so the group reads as a titled
// block rather than one more definition. The clauses sit under it (see
// legendGroup).
const tokenLegendTitle = "tokens:"

// tokenLegend spells out the token group's arithmetic, one clause per entry so
// each definition reads on its own. A legend, not just headers, because "in" is
// the total while "hit" and "miss" are its parts: the header alone cannot say how
// the total is counted. hit% caps at 99.9% (see maxHitRate); the legend keeps
// the short arithmetic and the cap lives here so the row width stays stable.
var tokenLegend = []legendEntry{
	{"in", "uncached input + cache read + cache write (total input)"},
	{"miss", "uncached input + cache write"},
	{"hit%", "hit / in"},
}

// detailHasTokens reports whether any model recorded tokens, so the token legend
// prints only when it explains a visible number, never beside a column of dashes.
func detailHasTokens(u *worktreeUsage) bool {
	if u == nil {
		return false
	}
	for _, h := range u.harnesses {
		for _, m := range h.models {
			if m.tokensKnown() {
				return true
			}
		}
	}
	return false
}

// tokenGroupKept reports whether the fitted grid still carries the token group,
// so the legend prints only when its columns do.
func tokenGroupKept(cols []worktreeColumn) bool {
	token := tokenHeaderSet()
	for _, c := range cols {
		if token[c.header] {
			return true
		}
	}
	return false
}

// printDetailLegend prints the breakdown's legend below its rows: the "~"
// estimate reasons its cells carry, what the LIST column holds, then the
// token-group arithmetic, in column order. Columns and their explanation
// therefore stay in one block, and a breakdown that keeps none prints nothing.
func printDetailLegend(reasons estimateReasons, list, partial, tokens bool, colorMode string, budget int) {
	if !reasons.any() && !list && !partial && !tokens {
		return
	}
	// Wrap to the grid's width, so the text that explains the block never widens
	// it; budget==0 (unbounded output) prints every clause whole.
	fmt.Println()
	printLegend(legendLines(detailLegendGroups(costLegend{tableReasons: reasons, list: list, partial: partial}, tokens), budget), colorMode)
}

// detailLegendGroups builds the cost head then the token group in print order.
func detailLegendGroups(l costLegend, tokens bool) []legendGroup {
	var groups []legendGroup
	if head := tableCostLegend(l); len(head.items) > 0 {
		groups = append(groups, head)
	}
	if tokens {
		groups = append(groups, legendGroup{title: tokenLegendTitle, items: tokenLegend})
	}
	return groups
}

// wrapWords breaks text into lines of at most width display cells on word
// boundaries. width<=0 or a line that already fits returns the text unchanged.
func wrapWords(text string, width int) []string {
	if width <= 0 || dispWidth(text) <= width {
		return []string{text}
	}
	var lines []string
	cur := ""
	for _, w := range strings.Fields(text) {
		switch {
		case cur == "":
			cur = w
		case dispWidth(cur)+1+dispWidth(w) <= width:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	return append(lines, cur)
}

// tierCount adapts one token tier to a detailMetric cell.
func tierCount(t tier) func(*modelUsage) string {
	return func(m *modelUsage) string { return tokenCount(m, t) }
}

// detailRows flattens the current worktree into one row per model, ordered by
// billed cost. Each row carries the model's time, its billed cost, the list
// price when the schema shows one, and one cell per token-group column, so the
// breakdown stays beside the model without harness grouping or sub-rows.
func detailRows(u *worktreeUsage, showList bool) [][]string {
	models := u.mergedModels()
	var rows [][]string
	for _, name := range u.sortedModelNames(models) {
		rows = append(rows, detailRow(name, models[name], showList))
	}
	return rows
}

// detailRow projects one model: name, time, billed cost, optional list price,
// then one cell per token-group column. The name is display text: ANSI
// sequences and newlines are stripped so a corrupt transcript model label can
// never inject styling or split the row; the JSON report keeps the raw model
// identity (see status_json.go).
func detailRow(name string, m *modelUsage, showList bool) []string {
	name = ansi.Strip(name)
	name = strings.Join(strings.Fields(name), " ")
	row := []string{name, modelTimeCell(m), m.cost.billed.cell()}
	if showList {
		row = append(row, listCell(m.cost))
	}
	for _, d := range detailTokenMeta {
		row = append(row, d.cell(m))
	}
	return row
}

// modelTimeCell owns the partial-time grammar: a partial total with no readable
// span is unknown, otherwise a marked duration.
func modelTimeCell(m *modelUsage) string {
	active := m.active()
	if !m.partial {
		return humanDuration(active)
	}
	if active == 0 {
		return unknownValue
	}
	return partialValue(humanDuration(active), true)
}

// partialTokenCell renders one token count from a total that may have lost
// records. present gates presence; value is shown. A missing value with loss is
// unknown, a readable partial value carries the "~" marker, and a whole total
// keeps the existing "-"/count grammar.
func partialTokenCell(value int64, present, partial bool) string {
	if partial {
		if !present {
			return unknownValue
		}
		return partialValue(formatTokens(value), true)
	}
	if !present {
		return dashValue
	}
	return formatTokens(value)
}

// tokenCount renders one tier's count: the compact form when the tier recorded
// tokens, "-" when none, and "unknown" when the count was rejected.
func tokenCount(m *modelUsage, t tier) string {
	if m.tokens.corrupt.has(t) {
		return unknownValue
	}
	n := m.tokens.tier(t)
	return partialTokenCell(n, n > 0, m.partial)
}

// totalInputCell renders the model's total prompt tokens (uncached input, cache
// reads and cache writes): the base every cache share is taken from. "-" when the
// model recorded no prompt tokens, "unknown" when a prompt class is corrupt.
func totalInputCell(m *modelUsage) string {
	if m.tokens.corruptPrompt() {
		return unknownValue
	}
	context := m.tokens.context()
	return partialTokenCell(context, context != 0, m.partial)
}

// missCount renders the miss subtotal: "-" when the model recorded no prompt
// tokens, else the compact count. A zero beside a nonzero hit is an honest sign
// that the build left the recorded window (see hitRateCell); a corrupt prompt
// class reads unknown rather than a zero placeholder.
func missCount(m *modelUsage) string {
	if m.tokens.corruptPrompt() {
		return unknownValue
	}
	context := m.tokens.context()
	return partialTokenCell(m.tokens.miss(), context != 0, m.partial)
}

// hitRateCell renders one model's cache-hit share: the percent, "unknown" when
// hits have no recorded build or a prompt class is corrupt, and "-" when the
// model recorded no prompt tokens at all.
func hitRateCell(m *modelUsage) string {
	if m.tokens.corruptPrompt() {
		return unknownValue
	}
	rate, ok := m.tokens.hitRate()
	if ok {
		return partialValue(formatPercent(rate), m.partial)
	}
	if m.partial {
		return unknownValue
	}
	if m.tokens.context() == 0 {
		return dashValue
	}
	return unknownValue
}
