package main

import (
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

// statusCellStyle maps a status-table cell to its style. Basic ANSI colors are
// deliberate: the terminal theme owns the palette, and every profile (16/256/
// truecolor/NO_COLOR) degrades cleanly. UNUSED is Yellow, not Red: it is the
// gc candidate, not an error.
func statusCellStyle(header, value string) lipgloss.Style {
	switch header {
	case "STATUS":
		if value == "ACTIVE" {
			return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Green)
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	case "TEMP":
		return tempCellStyle(value)
	case "CONFIG", "DB", "MCP":
		return booleanStyle(value == "yes")
	case "UPSTREAM":
		return booleanStyle(value != "-")
	case "BRANCH", "LAST USED":
		if value == "(detached)" || value == "never" {
			return lipgloss.NewStyle().Faint(true)
		}
	}
	return lipgloss.NewStyle()
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

// cellStyleFunc maps a column header and plain cell value to its style.
type cellStyleFunc func(header, value string) lipgloss.Style

// printTable renders a width-fitted table. Deterministic output is a test
// contract: the budget comes from COLUMNS or a TTY only, and cells are sized
// and middle-elided here — never by the table resizer, whose end-elision and
// shrink-column choice we cannot observe or pin. Styles are applied only when
// the resolved profile supports them; NoTTY output stays pure ASCII so pipes,
// --json and goldens remain byte-stable.
func printTable(headers []string, rows [][]string, colorMode string, style cellStyleFunc, rightAlign []string) {
	headers, rows, widths, aligns := sizedGrid(headers, rows, widthBudget(), rightAlign)
	profile := resolveColorProfile(colorMode)
	if profile > colorprofile.NoTTY && style != nil {
		headers, rows = styleCells(headers, rows, style)
	}
	header, body := renderGrid(headers, rows, widths, aligns)
	writeTable(gridString(header, body), profile)
}

// sizedGrid resolves a grid against a width budget: columns are sized, then
// middle-elided, and alignment is resolved from the plain headers before
// elision so ANSI decoration can never change which columns are right-aligned.
// Return order matches renderGrid's parameters, so renderGrid(sizedGrid(...))
// composes.
func sizedGrid(headers []string, rows [][]string, budget int, rightAlign []string) ([]string, [][]string, []int, []lipgloss.Position) {
	widths := columnWidths(headers, rows, budget)
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

// styleCells pre-renders the header row bold and each body cell through style.
// It runs after elision so widths are measured on plain text.
func styleCells(headers []string, rows [][]string, style cellStyleFunc) ([]string, [][]string) {
	styled := make([]string, len(headers))
	for i, h := range headers {
		styled[i] = lipgloss.NewStyle().Bold(true).Render(h)
	}
	out := make([][]string, len(rows))
	for r, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = style(headers[i], c).Render(c)
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
// shrinks the shrinkPriority columns in order down to their floor and, only if
// that is still not enough, down to a single cell. Pinned columns never change.
func columnWidths(headers []string, rows [][]string, budget int) []int {
	widths := naturalWidths(headers, rows)
	if budget <= 0 || lineWidth(widths) <= budget {
		return widths
	}
	over := lineWidth(widths) - budget
	for _, name := range shrinkPriority {
		over = shrinkColumn(widths, slices.Index(headers, name), over, shrinkFloor[name])
	}
	if over > 0 {
		for _, name := range shrinkPriority {
			over = shrinkColumn(widths, slices.Index(headers, name), over, 1)
		}
	}
	return widths
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

// lineWidth is a rendered row's width: cells plus the two-cell gutters.
func lineWidth(widths []int) int {
	total := 0
	for _, w := range widths {
		total += w
	}
	if len(widths) > 1 {
		total += 2 * (len(widths) - 1)
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
