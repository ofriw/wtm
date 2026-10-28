package main

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// ui.go — table rendering and the interactive gc picker.

// warnPrefix labels every non-fatal disclosure line, wherever it is emitted.
const warnPrefix = "warning: "

// warnf writes a `warning:` line to stderr. stdout stays machine-clean, so
// every non-fatal disclosure goes through here.
func warnf(format string, a ...any) {
	fmt.Fprint(os.Stderr, warnPrefix+fmt.Sprintf(format, a...)+"\n")
}

// pickerOptionPrefixWidth is how far huh's MultiSelect indents an option key:
// the selector ("> " / "  ") plus the toggle prefix ("• " / "✓ ").
const pickerOptionPrefixWidth = 4

// pickerChromeWidth is ThemeCharm's frame around a field's content: the left
// ThickBorder edge plus PaddingLeft(1). A picker row is therefore
// pickerChromeWidth + pickerOptionPrefixWidth wider than its label.
// Coupled to huh's ThemeCharm: changing the theme requires updating these
// widths and TestGCPickerNarrowWidth.
const pickerChromeWidth = 2

// Color modes accepted by --color; "" behaves as auto.
const (
	colorAuto   = "auto"
	colorAlways = "always"
	colorNever  = "never"
)

func booleanStyle(active bool) lipgloss.Style {
	if active {
		return lipgloss.NewStyle().Foreground(lipgloss.Green)
	}
	return lipgloss.NewStyle().Faint(true)
}

// titleStyle is the one title treatment: every table header and the block
// title share it, so "title" reads the same everywhere and outranks both the
// plain data and the faint secondary labels. Basic ANSI cyan is deliberate:
// the terminal theme owns the exact hue.
func titleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Cyan)
}

// secondaryValue reports a rendered non-value: absent ("-") or unpriced
// ("unknown"). One rule serves both the table cells and the summary block, so
// a non-value recedes with faint everywhere.
func secondaryValue(v string) bool { return v == "-" || v == "unknown" }

// statusCellStyle maps a status-table cell to its style. Basic ANSI colors are
// deliberate: the terminal theme owns the palette, and every profile (16/256/
// truecolor/NO_COLOR) degrades cleanly. UNUSED is Yellow, not Red: it is the
// gc candidate, not an error.
func statusCellStyle(header, value string) lipgloss.Style {
	if style, ok := statusColumnStyle(header, value); ok {
		return style
	}
	// Capability columns only appear in the opt-in --wide view.
	if isCapabilityHeader(header) {
		return capabilityCellStyle(value)
	}
	return lipgloss.NewStyle()
}

// statusColumnStyle maps the fixed status columns. The ok result is false when
// the header has no rule, so the caller can fall through to the capability
// columns.
func statusColumnStyle(header, value string) (lipgloss.Style, bool) {
	switch header {
	case "STATUS":
		return statusWordStyle(value), true
	case "TEMP":
		return tempCellStyle(value), true
	case "UPSTREAM":
		return booleanStyle(value != "-"), true
	case "COST", "LIST", "BRANCH", "LAST USED":
		return secondaryColumnStyle(header, value)
	}
	return lipgloss.Style{}, false
}

// statusWordStyle colors the STATUS word: ACTIVE is live, anything else is the
// gc candidate.
func statusWordStyle(value string) lipgloss.Style {
	if value == "ACTIVE" {
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Green)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Yellow)
}

// secondaryColumnStyle fades a COST/LIST non-value and a stale BRANCH/LAST USED
// marker; any other value stays plain. The ok result is false so the caller can
// try the capability columns.
func secondaryColumnStyle(header, value string) (lipgloss.Style, bool) {
	switch header {
	case "COST", "LIST":
		// A number is the signal; "unknown" and "-" are non-values and recede.
		if secondaryValue(value) {
			return lipgloss.NewStyle().Faint(true), true
		}
	case "BRANCH", "LAST USED":
		if value == "(detached)" || value == "never" {
			return lipgloss.NewStyle().Faint(true), true
		}
	}
	return lipgloss.Style{}, false
}

// capabilityCellStyle maps a --wide capability cell. The state word is the
// state, so it maps straight to the one semantic color scheme instead of
// through a value-string wrapper.
func capabilityCellStyle(value string) lipgloss.Style {
	switch value {
	case "yes":
		return capStateStyle(capPresent)
	case "partial":
		return capStateStyle(capPartial)
	default: // "-" and "n/a"
		return capStateStyle(capAbsent)
	}
}

func tempCellStyle(value string) lipgloss.Style {
	if value == "-" {
		return lipgloss.NewStyle().Faint(true)
	}
	if value == "expired" {
		return lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Cyan)
}

// capStateStyle is the one semantic color scheme: present green, partial yellow,
// absent/n-a faint. A word or a shape carries the state; color only reinforces it.
func capStateStyle(s capState) lipgloss.Style {
	switch s {
	case capPresent:
		return lipgloss.NewStyle().Foreground(lipgloss.Green)
	case capPartial:
		return lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	default:
		return lipgloss.NewStyle().Faint(true)
	}
}

// cellRenderer renders one plain cell value into its colored form. printTable
// calls it only when the profile supports styling, so it may always decorate.
type cellRenderer func(header, value string) string

// statusCell renders one plain status cell with its color. The grouped
// INTEGRATIONS cell colors token-by-token; every other column takes one style.
func statusCell(header, value string) string {
	if header == colIntegrations.header {
		return groupedCellStyle(value)
	}
	return statusCellStyle(header, value).Render(value)
}

// legendStyle is the one treatment for the explanatory lines under a table:
// dim, because a legend is secondary to the data it explains. Basic dim
// degrades cleanly across every profile.
func legendStyle() lipgloss.Style { return lipgloss.NewStyle().Faint(true) }

// printLegend writes explanatory lines below a table through the resolved
// profile, so a legend dims on a color terminal and stays byte-clean when the
// output is piped. Styling is skipped on NoTTY, matching printTable, so pipes,
// --json and goldens remain byte-stable.
func printLegend(lines []string, colorMode string) {
	profile := resolveColorProfile(colorMode)
	w := &colorprofile.Writer{Forward: os.Stdout, Profile: profile}
	for _, line := range lines {
		if profile > colorprofile.NoTTY {
			line = legendStyle().Render(line)
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

// groupedCellStyle colors a grouped INTEGRATIONS cell token by token: present
// green, partial yellow, absent faint. Shape (via the shared markers) carries
// the state; color only reinforces it.
func groupedCellStyle(value string) string {
	if value == "-" {
		return capStateStyle(capAbsent).Render(value)
	}
	tokens := strings.Fields(value)
	for i, tok := range tokens {
		// WHY: the ~ suffix groupedToken writes is the state, so color keys
		// off it directly instead of through a token-string wrapper.
		state := capPresent
		if strings.HasSuffix(tok, partialMarker) {
			state = capPartial
		}
		tokens[i] = capStateStyle(state).Render(tok)
	}
	return strings.Join(tokens, " ")
}

// printTable renders a width-fitted table. Deterministic output is a test
// contract: the caller supplies the budget, and cells are sized
// and middle-elided here — never by the table resizer, whose end-elision and
// shrink-column choice we cannot observe or pin. Styles are applied only when
// the resolved profile supports them; NoTTY output stays pure ASCII so pipes,
// --json and goldens remain byte-stable.
func printTable(headers []string, rows [][]string, colorMode string, render cellRenderer, rightAlign []string, budget int) {
	headers, rows, widths, aligns := sizedGrid(headers, rows, budget, rightAlign)
	profile := resolveColorProfile(colorMode)
	// A nil renderer renders plain text: the guard keeps that a no-op instead of
	// relying on the profile to skip the call.
	if profile > colorprofile.NoTTY && render != nil {
		headers, rows = renderCells(headers, rows, render)
	}
	header, body := renderGrid(headers, rows, widths, aligns)
	writeTable(gridString(header, body), profile)
}

// printKeyValues renders a headerless two-column block: label, then value. It
// shares renderTable's sizing, two-cell gutter, trimming and width budget with
// printTable, so a key/value block lines up with the tables around it instead
// of introducing a third layout. The first titleRows labels carry the title
// style; the rest are faint; values stay plain except a non-value ("-"/
// "unknown"), which recedes with faint. Styles apply after sizing and elision,
// and NoTTY output is ASCII-only, so goldens stay byte-stable.
func printKeyValues(rows [][]string, titleRows int, colorMode string, budget int) {
	labels, widths := sizedKeyValues(rows, budget)
	aligns := columnAligns(labels, nil)        // key/value blocks are left-aligned
	_, rows = elideCells(labels, rows, widths) // elide plain text before styling
	profile := resolveColorProfile(colorMode)
	if profile > colorprofile.NoTTY {
		rows = styleKeyValues(rows, titleRows)
	}
	writeTable(renderTable(nil, rows, widths, aligns), profile)
}

// keyValueValueFloor is the narrowest value cell a summary block keeps. Below it
// a rendered value is a bare ellipsis, which reads as missing, not elided.
const keyValueValueFloor = 6

// sizedKeyValues sizes a two-column summary block to the terminal budget. The
// block renders headerless, so an empty label slice stands in for the width
// pass; callers with no rows must not call it.
func sizedKeyValues(rows [][]string, budget int) ([]string, []int) {
	labels := make([]string, len(rows[0])) // width pass only; no header row renders
	return labels, fitKeyValues(naturalWidths(labels, rows), budget)
}

// fitKeyValues fits a two-column summary block to the budget: the value column
// gives up width first, then the label, so a long path cannot wrap the block
// while the table above elides. Values never fall below keyValueValueFloor.
func fitKeyValues(widths []int, budget int) []int {
	if budget <= 0 || lineWidth(widths) <= budget {
		return widths
	}
	widths[1] = max(keyValueValueFloor, widths[1]-(lineWidth(widths)-budget))
	if over := lineWidth(widths) - budget; over > 0 {
		widths[0] = max(1, widths[0]-over)
	}
	return widths
}

// styleKeyValues title-styles the first titleRows labels and faints the
// remaining ones. Labels identify data; values are data, so a value stays
// plain. A non-value ("-"/"unknown") recedes with faint, matching the table
// cells (see statusCellStyle's COST case).
func styleKeyValues(rows [][]string, titleRows int) [][]string {
	out := make([][]string, len(rows))
	for i, r := range rows {
		label := lipgloss.NewStyle().Faint(true)
		if i < titleRows {
			label = titleStyle()
		}
		value := lipgloss.NewStyle()
		if secondaryValue(r[1]) {
			value = value.Faint(true)
		}
		out[i] = []string{label.Render(r[0]), value.Render(r[1])}
	}
	return out
}

// sizedGrid resolves a grid against a width budget: columns are sized, then
// middle-elided, and alignment is resolved from the plain headers before
// elision so ANSI decoration can never change which columns are right-aligned.
// Return order matches renderGrid's parameters, so renderGrid(sizedGrid(...))
// composes.
func sizedGrid(headers []string, rows [][]string, budget int, rightAlign []string) ([]string, [][]string, []int, []lipgloss.Position) {
	widths := columnWidths(headers, rows, budget)
	headers, rows, widths = dropZeroColumns(headers, rows, widths)
	aligns := columnAligns(headers, rightAlign)
	headers, rows = elideCells(headers, rows, widths)
	return headers, rows, widths, aligns
}

// renderGrid splits a rendered grid into its header line ("" when headers is
// empty) and its body lines. gridString is the exact inverse, so a grid always
// round-trips through the split.
func renderGrid(headers []string, rows [][]string, widths []int, aligns []lipgloss.Position) (string, []string) {
	lines := strings.Split(renderTable(headers, rows, widths, aligns), "\n")
	if len(headers) == 0 {
		return "", lines
	}
	return lines[0], lines[1:]
}

// gridString rejoins a header line and body lines into one rendered grid.
func gridString(header string, body []string) string {
	if header == "" {
		return strings.Join(body, "\n")
	}
	return strings.Join(append([]string{header}, body...), "\n")
}

// columnAligns maps right-align header names to positions, defaulting left.
func columnAligns(headers []string, rightAlign []string) []lipgloss.Position {
	aligns := make([]lipgloss.Position, len(headers))
	for i, h := range headers {
		if slices.Contains(rightAlign, h) {
			aligns[i] = lipgloss.Right
		} else {
			aligns[i] = lipgloss.Left
		}
	}
	return aligns
}

// resolveColorProfile is the single source of truth for color precedence:
// --color beats NO_COLOR, which beats FORCE_COLOR/CLICOLOR_FORCE, then the
// TERM/CLICOLOR/isatty capability detection in colorprofile.Detect.
func resolveColorProfile(mode string) colorprofile.Profile {
	return resolveColorMode(mode, term.IsTerminal(os.Stdout.Fd()))
}

func envColorProfile() colorprofile.Profile {
	if truthy(os.Getenv("FORCE_COLOR")) {
		return max(colorprofile.Detect(os.Stdout, os.Environ()), colorprofile.ANSI)
	}
	if os.Getenv("CLICOLOR") == "0" && !truthy(os.Getenv("CLICOLOR_FORCE")) {
		return colorprofile.NoTTY
	}
	return colorprofile.Detect(os.Stdout, os.Environ())
}

func resolveColorMode(mode string, isTTY bool) colorprofile.Profile {
	switch mode {
	case colorAlways:
		return colorprofile.TrueColor
	case colorNever:
		return colorprofile.NoTTY
	}
	if noColorSet() {
		if isTTY {
			return colorprofile.ASCII
		}
		return colorprofile.NoTTY
	}
	return envColorProfile()
}

// validateColor rejects an unknown --color value so it can never silently
// fall back to auto.
func validateColor(mode string) error {
	switch mode {
	case "", colorAuto, colorAlways, colorNever:
		return nil
	}
	return usagef("--color: want auto, always or never, got %q", mode)
}

// noColorSet implements https://no-color.org: any non-empty NO_COLOR disables
// color, including values colorprofile's ParseBool would read as false.
func noColorSet() bool {
	v, ok := os.LookupEnv("NO_COLOR")
	return ok && v != ""
}

func truthy(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// renderCells title-styles the header row and renders each body cell through
// render. It runs after elision so widths are measured on plain text.
func renderCells(headers []string, rows [][]string, render cellRenderer) ([]string, [][]string) {
	styled := make([]string, len(headers))
	for i, h := range headers {
		styled[i] = titleStyle().Render(h)
	}
	out := make([][]string, len(rows))
	for r, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = render(headers[i], c)
		}
		out[r] = cells
	}
	return styled, out
}

// writeTable emits through a profile writer so colors are downsampled to the
// terminal, or stripped entirely on NoTTY, before reaching stdout.
func writeTable(out string, profile colorprofile.Profile) {
	w := &colorprofile.Writer{Forward: os.Stdout, Profile: profile}
	_, _ = fmt.Fprintln(w, out)
}

// widthBudget is the maximum rendered line width: a positive COLUMNS wins so
// callers and goldens can pin a width without a TTY; otherwise the stdout TTY
// width; otherwise 0, meaning unbounded.
func widthBudget() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	if term.IsTerminal(os.Stdout.Fd()) {
		if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
			return w
		}
	}
	return 0
}

func dispWidth(s string) int { return ansi.StringWidth(s) }

func balanceMiddle(s string, avail int) (head, tail string) {
	head = ansi.Truncate(s, avail/2, "")
	rest := s[len(head):]
	tail = ansi.TruncateLeft(rest, dispWidth(rest)-(avail-avail/2), "")
	// Wide graphemes make a grapheme-aware cut land past the target; trim the
	// head first, then the tail, so the result never exceeds avail.
	if excess := dispWidth(head) + dispWidth(tail) - avail; excess > 0 {
		head = ansi.Truncate(head, dispWidth(head)-excess, "")
	}
	if over := dispWidth(head) + dispWidth(tail) - avail; over > 0 {
		tail = ansi.TruncateLeft(tail, over, "")
	}
	return head, tail
}

// middleElide shortens s to at most w display cells, keeping both ends and
// replacing the middle with an ellipsis. Returns s unchanged when it fits.
func middleElide(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if dispWidth(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	head, tail := balanceMiddle(s, w-1)
	elided := head + "…" + tail
	// A single wide grapheme cannot be cut to fit a one-cell remainder; fall
	// back to end-elision, which drops whole graphemes until it fits.
	if dispWidth(elided) > w {
		return ansi.Truncate(s, w, "…")
	}
	return elided
}

// columnWidths sizes each column to natural width when the budget allows, else
// gives up width in rank order. A declared floor is a hard minimum: the shrink
// pass stops there, so a measurement column keeps its whole value. A zero-floor
// column drops whole, including a zero-floor unelidable column such as LIST. A
// positive-floor unelidable column (AGE, ACTIVE, LAST USED, INTEGRATIONS, TIME,
// COST) is pinned at natural width and drops whole only in the overflow pass.
// The table never shows a bare ellipsis or a truncated measurement; a long
// elidable identity column may still middle-elide to its column width. Shrink
// priority is derived from the actual headers via columnRegistry(), so --wide
// capability columns participate alongside the core columns.
func columnWidths(headers []string, rows [][]string, budget int) []int {
	widths := naturalWidths(headers, rows)
	if budget <= 0 || lineWidth(widths) <= budget {
		return widths
	}
	registry := columnRegistry()
	priority := dynamicShrinkPriority(headers, registry)
	floors := dynamicShrinkFloor(headers, registry)
	pinUnelidableFloors(headers, widths, registry, floors)
	over := shrinkToFloors(headers, widths, priority, floors, budget)
	dropOverflowColumns(headers, widths, priority, budget, over)
	return widths
}

// pinUnelidableFloors pins each unelidable column with a floor at its natural
// width: the shrink pass then cannot cut a measurement value, and
// dropOverflowColumns drops the whole column instead. A floor-0 unelidable
// column has no useful minimum, so it drops whole in the shrink pass at its
// rank; pinning it would make it outlive cheaper columns (LIST did before this
// distinction).
func pinUnelidableFloors(headers []string, widths []int, registry map[string]worktreeColumn, floors map[string]int) {
	for i, h := range headers {
		if c, ok := registry[h]; ok && c.unelidable && c.floor > 0 {
			floors[h] = max(floors[h], widths[i])
		}
	}
}

// dropOverflowColumns is the last resort when the declared floors still do not
// fit: drop whole columns (reclaiming their gutter) in the same priority order,
// instead of rendering an unlabelled sliver or a cut measurement. Unelidable
// columns reach this stage at natural width, so a measurement column is dropped
// whole with the stderr disclosure rather than middle-elided. Pinned columns
// (rank 0) are absent from priority, so when the budget cannot hold even the
// pinned set they are dropped tail-first: the grid must fit its budget, and the
// leading pinned columns (the state) outlive the trailing ones (the temp
// countdown).
func dropOverflowColumns(headers []string, widths []int, priority []string, budget, over int) {
	for _, name := range priority {
		if over <= 0 {
			break
		}
		i := slices.Index(headers, name)
		if i < 0 || widths[i] == 0 {
			continue
		}
		widths[i] = 0
		over = lineWidth(widths) - budget
	}
	for i := len(widths) - 1; i >= 0 && over > 0; i-- {
		if widths[i] == 0 {
			continue
		}
		widths[i] = 0
		over = lineWidth(widths) - budget
	}
}

// shrinkToFloors gives up width in rank order until the budget fits. A
// zero-floor column drops whole (it has no useful minimum), whether or not it is
// unelidable; every other column stops at its floor, which positive-floor
// unelidable columns pin at natural width. The returned over is what the floors
// cannot absorb; dropOverflowColumns then drops whole columns.
func shrinkToFloors(headers []string, widths []int, priority []string, floors map[string]int, budget int) int {
	over := lineWidth(widths) - budget
	for _, name := range priority {
		if over <= 0 {
			break
		}
		i := slices.Index(headers, name)
		if floors[name] == 0 {
			// Dropping a column also reclaims its gutter.
			widths[i] = 0
			over = lineWidth(widths) - budget
		} else {
			over = shrinkColumn(widths, i, over, floors[name])
		}
	}
	return over
}

// dropZeroColumns removes columns sized to zero, so a dropped column leaves no
// gutter or empty cell behind. Cells, widths and headers stay index-aligned.
// Dropped columns are disclosed on stderr (stdout stays machine-clean for pipes
// and goldens), so a missing column reads as hidden, not absent.
func dropZeroColumns(headers []string, rows [][]string, widths []int) ([]string, [][]string, []int) {
	keep := positiveWidthColumns(widths)
	if len(keep) == len(widths) {
		return headers, rows, widths
	}
	hidden := hiddenHeaders(headers, widths)
	if len(hidden) > 0 {
		warnf("terminal too narrow; hid %s; widen the terminal or use --json", strings.Join(hidden, ", "))
	}
	out := make([][]string, len(rows))
	for r, row := range rows {
		out[r] = selectStrings(row, keep)
	}
	return selectStrings(headers, keep), out, selectInts(widths, keep)
}

func positiveWidthColumns(widths []int) []int {
	var keep []int
	for i, w := range widths {
		if w > 0 {
			keep = append(keep, i)
		}
	}
	return keep
}

// hiddenHeaders names the columns dropped from a grid, so a missing column
// reads as hidden, not absent.
func hiddenHeaders(headers []string, widths []int) []string {
	var hidden []string
	for i, h := range headers {
		if widths[i] == 0 {
			hidden = append(hidden, h)
		}
	}
	return hidden
}

// selectStrings keeps the columns at keep indices. Concrete (not generic): only
// dropZeroColumns calls these, so one tiny function per element type is simpler
// than a type parameter.
func selectStrings(values []string, keep []int) []string {
	out := make([]string, len(keep))
	for j, i := range keep {
		out[j] = values[i]
	}
	return out
}

func selectInts(values []int, keep []int) []int {
	out := make([]int, len(keep))
	for j, i := range keep {
		out[j] = values[i]
	}
	return out
}

// dynamicShrinkPriority returns the headers that can shrink, sorted by rank,
// drawn from registry so --wide capability columns participate.
func dynamicShrinkPriority(headers []string, registry map[string]worktreeColumn) []string {
	type ranked struct {
		header string
		rank   int
	}
	var shrinkable []ranked
	for _, h := range headers {
		if c, ok := registry[h]; ok && c.shrinkRank > 0 {
			shrinkable = append(shrinkable, ranked{h, c.shrinkRank})
		}
	}
	sort.SliceStable(shrinkable, func(i, j int) bool { return shrinkable[i].rank < shrinkable[j].rank })
	out := make([]string, len(shrinkable))
	for i, r := range shrinkable {
		out[i] = r.header
	}
	return out
}

// dynamicShrinkFloor returns the floor for each shrinkable header, drawn from
// registry so --wide capability columns participate.
func dynamicShrinkFloor(headers []string, registry map[string]worktreeColumn) map[string]int {
	floors := make(map[string]int)
	for _, h := range headers {
		if c, ok := registry[h]; ok && c.shrinkRank > 0 {
			floors[h] = c.floor
		}
	}
	return floors
}

func naturalWidths(headers []string, rows [][]string) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = dispWidth(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if w := dispWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	return widths
}

// lineWidth is a rendered row's width: cells plus the two-cell gutters. A
// zero-width column is dropped on render, so it adds neither cells nor gutter.
func lineWidth(widths []int) int {
	total, n := 0, 0
	for _, w := range widths {
		if w > 0 {
			total += w
			n++
		}
	}
	if n > 1 {
		total += 2 * (n - 1)
	}
	return total
}

// shrinkColumn reduces column i toward floor until over is spent; an absent
// column (i < 0) is a no-op. Returns the unspent over.
func shrinkColumn(widths []int, i, over, floor int) int {
	if i < 0 || over <= 0 {
		return over
	}
	reduce := min(widths[i]-floor, over)
	if reduce > 0 {
		widths[i] -= reduce
		over -= reduce
	}
	return over
}

// elideCells middle-elides every header and cell that exceeds its column width.
func elideCells(headers []string, rows [][]string, widths []int) ([]string, [][]string) {
	elided := make([]string, len(headers))
	for i, h := range headers {
		elided[i] = middleElide(h, widths[i])
	}
	out := make([][]string, len(rows))
	for r, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = middleElide(c, widths[i])
		}
		out[r] = cells
	}
	return elided, out
}

// renderTable joins pre-sized cells with two-cell gutters. It never calls the
// table-level Width, whose resizer would end-elide and choose its own shrink
// columns; per-column widths come from StyleFunc (whose Width includes
// padding). Wrap(false) is required or oversize cells wrap to a second line.
func renderTable(headers []string, rows [][]string, widths []int, aligns []lipgloss.Position) string {
	styleFunc := func(_, col int) lipgloss.Style {
		s := lipgloss.NewStyle().AlignHorizontal(aligns[col])
		if col == len(widths)-1 {
			return s.Width(widths[col])
		}
		return s.PaddingRight(2).Width(widths[col] + 2)
	}
	out := table.New().
		BorderTop(false).BorderBottom(false).BorderLeft(false).
		BorderRight(false).BorderHeader(false).BorderRow(false).BorderColumn(false).
		Wrap(false).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(styleFunc).
		Render()
	return trimTrailingSpaces(out)
}

// trimTrailingSpaces restores the no-trailing-padding contract: the table pads
// the last column to its width, but a golden or diff must not carry it.
func trimTrailingSpaces(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// interactive reports whether stdin and stdout are both terminals.
func interactive() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// multiSelect shows candidates: nothing selected by default, ctrl+a selects
// all (huh default keymap), space toggles. header becomes the field description
// and must already carry the indent that lines it up with the option keys; an
// empty header is a huh no-op. Returns indices into labels.
func multiSelect(title, header string, labels []string) ([]int, error) {
	opts := make([]huh.Option[int], len(labels))
	for i, l := range labels {
		opts[i] = huh.NewOption(l, i)
	}
	var sel []int
	err := huh.NewMultiSelect[int]().
		Title(title).
		Description(header).
		Options(opts...).
		Value(&sel).
		Run()
	return sel, err
}

// confirm asks a yes/no question; def is the preselected answer.
func confirm(title string, def bool) (bool, error) {
	v := def
	err := huh.NewConfirm().
		Title(title).
		Affirmative("yes").
		Negative("no").
		Value(&v).
		Run()
	return v, err
}
