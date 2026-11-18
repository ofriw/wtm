package cli

import (
	"strings"
	"testing"
)

// terminalQueries are the reply-expecting sequences that must never reach the
// progress stream: input is disabled (tea.WithInput(nil)), so nothing can read
// the terminal's answer and it would sit in the tty until the shell echoed it
// (charmbracelet/bubbletea#1590).
var terminalQueries = []string{"\x1b[?2026$p", "\x1b[?2027$p", "\x1b[?u"}

// assertNoTerminalQueries pins the input-disabled contract on a captured
// progress stream.
func assertNoTerminalQueries(t *testing.T, stream string) {
	t.Helper()
	for _, q := range terminalQueries {
		if strings.Contains(stream, q) {
			t.Errorf("progress stream carries terminal query %q: its reply would leak to the shell prompt\nstream:\n%s", q, stream)
		}
	}
}
