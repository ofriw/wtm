package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// ui.go — generic table rendering, color resolution and the interactive
// pickers. It does not know the domain schema: column IDs, capability headers
// and state words arrive as data (see core.ColumnID* and core.StateWord*).
// Domain styling that colors specific status/state cells lives in
// internal/cli/status_cmd.go; ui only applies the caller-supplied renderer.

// WarnPrefix labels every non-fatal disclosure line, wherever it is emitted.
const WarnPrefix = "warning: "

// Warnf writes a `warning:` line to stderr. stdout stays machine-clean, so
// every non-fatal disclosure goes through here.
func Warnf(format string, a ...any) {
	fmt.Fprint(os.Stderr, WarnPrefix+fmt.Sprintf(format, a...)+"\n")
}

// PickerOptionPrefixWidth is how far huh's MultiSelect indents an option key:
// the selector ("> " / "  ") plus the toggle prefix ("• " / "✓ ").
const PickerOptionPrefixWidth = 4

// PickerChromeWidth is ThemeCharm's frame around a field's content: the left
// ThickBorder edge plus PaddingLeft(1). A picker row is therefore
// PickerChromeWidth + PickerOptionPrefixWidth wider than its label.
// Coupled to huh's ThemeCharm: changing the theme requires updating these
// widths and TestGCPickerNarrowWidth.
const PickerChromeWidth = 2

// Color modes accepted by --color; "" behaves as auto.
const (
	ColorAuto   = "auto"
	ColorAlways = "always"
	ColorNever  = "never"
)

// cellRenderer renders one plain cell value into its colored form. It keys off
// the column's stable ID (not its display header) so styling survives header
// renames. PrintTable calls it only when the profile supports styling, so it
// may always decorate.
type cellRenderer func(id, value string) string

// Policy bundles the shrink-policy inputs a grid needs: the elision priority
// and floors (derived from the caller's column registry) plus capHeader, which
// identifies capability headers so dropped --wide columns can be disclosed.
type Policy struct {
	Priority  []string
	Floors    map[string]int
	CapHeader func(string) bool
}

// PrintTable renders a width-fitted table. Deterministic output is a test
// contract: the budget comes from COLUMNS or a TTY only, and cells are sized
// and middle-elided here — never by the table resizer, whose end-elision and
// shrink-column choice we cannot observe or pin. Styles are applied only when
// the resolved profile supports them; NoTTY output stays pure ASCII so pipes,
// --json and goldens remain byte-stable. p carries the shrink policy from the
// caller's column registry and the capability-header identifier for the drop
// disclosure.
func PrintTable(headers, ids []string, rows [][]string, colorMode string, render cellRenderer, rightAlign []string, p Policy) {
	headers, ids, rows, widths, aligns := SizedGrid(headers, ids, rows, WidthBudget(), rightAlign, p)
	profile := resolveColorProfile(colorMode)
	if profile > colorprofile.NoTTY && render != nil {
		headers, rows = RenderCells(headers, ids, rows, render)
	}
	header, body := RenderGrid(headers, rows, widths, aligns)
	writeTable(GridString(header, body), profile)
}

// SizedGrid resolves a grid against a width budget: columns are sized, then
// middle-elided, and alignment is resolved from the plain headers before
// elision. ids is pruned in lockstep with headers/rows, so renderers always
// see ids aligned to the kept columns.
func SizedGrid(headers, ids []string, rows [][]string, budget int, rightAlign []string, p Policy) ([]string, []string, [][]string, []int, []lipgloss.Position) {
	widths := ColumnWidths(headers, rows, budget, p.Priority, p.Floors, p.CapHeader)
	headers, ids, rows, widths = dropZeroColumns(headers, ids, rows, widths, p.CapHeader)
	aligns := ColumnAligns(headers, rightAlign)
	headers, rows = elideCells(headers, rows, widths)
	return headers, ids, rows, widths, aligns
}

// RenderGrid splits a rendered grid into its header line ("" when headers is
// empty) and its body lines. GridString is the exact inverse, so a grid always
// round-trips through the split.
func RenderGrid(headers []string, rows [][]string, widths []int, aligns []lipgloss.Position) (string, []string) {
	lines := strings.Split(RenderTable(headers, rows, widths, aligns), "\n")
	if len(headers) == 0 {
		return "", lines
	}
	return lines[0], lines[1:]
}

// GridString rejoins a header line and body lines into one rendered grid.
func GridString(header string, body []string) string {
	if header == "" {
		return strings.Join(body, "\n")
	}
	return strings.Join(append([]string{header}, body...), "\n")
}

// ColumnAligns maps right-align header names to positions, defaulting left.
func ColumnAligns(headers []string, rightAlign []string) []lipgloss.Position {
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
	case ColorAlways:
		return colorprofile.TrueColor
	case ColorNever:
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

// RenderCells bolds the header row and renders each body cell through render.
// It runs after elision so widths are measured on plain text. ids parallels
// headers and is what render receives. When render is non-nil, ids must have
// exactly len(headers) entries; a nil or short ids here panics with a clear
// message instead of an index-out-of-range.
func RenderCells(headers, ids []string, rows [][]string, render cellRenderer) ([]string, [][]string) {
	if render != nil && len(ids) != len(headers) {
		panic(fmt.Sprintf("ui.RenderCells: render set but ids length %d != headers length %d", len(ids), len(headers)))
	}
	styled := make([]string, len(headers))
	for i, h := range headers {
		styled[i] = lipgloss.NewStyle().Bold(true).Render(h)
	}
	out := make([][]string, len(rows))
	for r, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = render(ids[i], c)
		}
		out[r] = cells
	}
	return styled, out
}

// PrintJSON writes indented JSON to stdout (machine output contract). Core's
// PrintStatusJSON and cli handlers share it so stdout stays the single machine
// channel; placement here keeps filesystem helpers free of presentation IO.
func PrintJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeTable emits through a profile writer so colors are downsampled to the
// terminal, or stripped entirely on NoTTY, before reaching stdout.
func writeTable(out string, profile colorprofile.Profile) {
	w := &colorprofile.Writer{Forward: os.Stdout, Profile: profile}
	_, _ = fmt.Fprintln(w, out)
}

// WidthBudget is the maximum rendered line width: a positive COLUMNS wins so
// callers and goldens can pin a width without a TTY; otherwise the stdout TTY
// width; otherwise 0, meaning unbounded.
func WidthBudget() int {
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

// DispWidth returns the display width of s in terminal cells (ANSI-aware).
func DispWidth(s string) int { return ansi.StringWidth(s) }

func balanceMiddle(s string, avail int) (head, tail string) {
	head = ansi.Truncate(s, avail/2, "")
	rest := s[len(head):]
	tail = ansi.TruncateLeft(rest, DispWidth(rest)-(avail-avail/2), "")
	// Wide graphemes make a grapheme-aware cut land past the target; trim the
	// head first, then the tail, so the result never exceeds avail.
	if excess := DispWidth(head) + DispWidth(tail) - avail; excess > 0 {
		head = ansi.Truncate(head, DispWidth(head)-excess, "")
	}
	if over := DispWidth(head) + DispWidth(tail) - avail; over > 0 {
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
	if DispWidth(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	head, tail := balanceMiddle(s, w-1)
	elided := head + "…" + tail
	// A single wide grapheme cannot be cut to fit a one-cell remainder; fall
	// back to end-elision, which drops whole graphemes until it fits.
	if DispWidth(elided) > w {
		return ansi.Truncate(s, w, "…")
	}
	return elided
}

// columnWidths sizes each column to natural width when the budget allows, else
// shrinks the shrinkable columns in rank order down to their floor and, only
// if that is still not enough, down to a single cell. Pinned columns never
// change. A floor-0 column is all-or-nothing: it renders at natural width or
// is dropped whole, never elided to a useless sliver. priority/floors derive
// from the caller's column registry, so --wide capability columns participate
// alongside the core columns.
func ColumnWidths(headers []string, rows [][]string, budget int, priority []string, floors map[string]int, capHeader func(string) bool) []int {
	widths := NaturalWidths(headers, rows)
	if budget <= 0 || LineWidth(widths) <= budget {
		return widths
	}
	over := shrinkToFloors(headers, widths, priority, floors, budget)
	if over > 0 {
		for _, name := range priority {
			over = shrinkColumn(widths, slices.Index(headers, name), over, 1)
		}
	}
	return widths
}

func shrinkToFloors(headers []string, widths []int, priority []string, floors map[string]int, budget int) int {
	over := LineWidth(widths) - budget
	for _, name := range priority {
		if over <= 0 {
			break
		}
		i := slices.Index(headers, name)
		if floors[name] == 0 {
			// Dropping a column also reclaims its gutter.
			widths[i] = 0
			over = LineWidth(widths) - budget
		} else {
			over = shrinkColumn(widths, i, over, floors[name])
		}
	}
	return over
}

// dropZeroColumns removes columns sized to zero, so a dropped column leaves no
// gutter or empty cell behind. Cells, widths and headers stay index-aligned.
// Capability columns dropped from a narrow --wide view are disclosed on stderr
// (stdout stays machine-clean for pipes and goldens), so a missing column reads
// as hidden, not absent. capHeader identifies those columns.
func dropZeroColumns(headers, ids []string, rows [][]string, widths []int, capHeader func(string) bool) ([]string, []string, [][]string, []int) {
	keep := positiveWidthColumns(widths)
	if len(keep) == len(widths) {
		return headers, ids, rows, widths
	}
	var hidden []string
	for i, h := range headers {
		if widths[i] == 0 && capHeader != nil && capHeader(h) {
			hidden = append(hidden, h)
		}
	}
	if len(hidden) > 0 {
		Warnf("terminal too narrow; hid %s; widen the terminal or drop --wide", strings.Join(hidden, ", "))
	}
	out := make([][]string, len(rows))
	for r, row := range rows {
		out[r] = selectStrings(row, keep)
	}
	// ids parallels headers; nil stays nil so a caller without per-cell ids
	// passes nil render as well (RenderCells requires ids when render is set).
	var idOut []string
	if ids != nil {
		idOut = selectStrings(ids, keep)
	}
	return selectStrings(headers, keep), idOut, out, selectInts(widths, keep)
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

// NaturalWidths measures each column's widest plain cell, including headers —
// the pre-elision width budget the grid sizing shrinks from.
func NaturalWidths(headers []string, rows [][]string) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = DispWidth(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if w := DispWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	return widths
}

// lineWidth is a rendered row's width: cells plus the two-cell gutters. A
// zero-width column is dropped on render, so it adds neither cells nor gutter.
func LineWidth(widths []int) int {
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
func RenderTable(headers []string, rows [][]string, widths []int, aligns []lipgloss.Position) string {
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

// Interactive reports whether stdin and stdout are both terminals.
func Interactive() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// MultiSelect shows candidates: nothing selected by default, ctrl+a selects
// all (huh default keymap), space toggles. header becomes the field description
// and must already carry the indent that lines it up with the option keys; an
// empty header is a huh no-op. Returns indices into labels.
func MultiSelect(title, header string, labels []string) ([]int, error) {
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

// Confirm asks a yes/no question; def is the preselected answer.
func Confirm(title string, def bool) (bool, error) {
	v := def
	err := huh.NewConfirm().
		Title(title).
		Affirmative("yes").
		Negative("no").
		Value(&v).
		Run()
	return v, err
}
