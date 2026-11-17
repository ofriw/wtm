package core

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"wtm/internal/ui"
)

// globals.go — the command framework shared by every subcommand: the global
// flag state, usage-error mapping and the flag helpers. It lives here (not in
// cli) because the status command — which needs all of it — also lives here,
// and cli imports core, never the reverse.

// UsageText is the full help text; cli installs it at init.
var UsageText = ""

// UsageError maps to exit code 2.
type UsageError string

func (e UsageError) Error() string { return string(e) }

// Usagef builds a UsageError.
func Usagef(format string, a ...any) error { return UsageError(fmt.Sprintf(format, a...)) }

// ErrAmbiguousBranch is a usage error so every command gives the same exit code
// when a request matches several branches only by case. The stored spelling is
// the repair, which makes the request a command-line mistake in all commands.
var ErrAmbiguousBranch = UsageError("matches multiple local branches by case")

// Globals are flags accepted both before and after the subcommand.
type Globals struct {
	Root     string
	JSON     bool
	Color    string
	Progress string
	Yes      bool
	Flags    map[string]bool // names registered here; subcommand help hides them
}

func (g *Globals) Register(fs *flag.FlagSet) {
	fs.StringVar(&g.Root, "root", g.Root, "repository root")
	fs.BoolVar(&g.JSON, "json", g.JSON, "machine-readable output")
	// Default is the current value: subFlags re-registers and must not clobber
	// a --color/--progress parsed before the subcommand.
	fs.StringVar(&g.Color, "color", g.Color, "color output: auto, always, never")
	fs.StringVar(&g.Progress, "progress", g.Progress, "progress display: auto, always, never")
	fs.BoolVar(&g.Yes, "yes", g.Yes, "never prompt")
	// register runs before a subcommand binds its own flags, so every flag here
	// is global. Deriving the set keeps the names in one place.
	g.Flags = map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { g.Flags[f.Name] = true })
}

// SubFlags builds a flag set for a subcommand; globals are re-registered so
// they may appear after the subcommand too (wtm status --json).
func SubFlags(name string, g *Globals) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.Register(fs)
	return fs
}

// PrintSubFlags shows a subcommand's own flag descriptions. The global block in
// usage is their single printed home, so globals are filtered out here.
func PrintSubFlags(fs *flag.FlagSet, g *Globals) {
	local := flag.NewFlagSet(fs.Name(), flag.ContinueOnError)
	fs.VisitAll(func(f *flag.Flag) {
		if g.Flags[f.Name] {
			return
		}
		local.Var(f.Value, f.Name, f.Usage)
		local.Lookup(f.Name).DefValue = f.DefValue
	})
	count := 0
	local.VisitAll(func(*flag.Flag) { count++ })
	if count == 0 {
		return
	}
	fmt.Printf("options for wtm %s:\n", fs.Name())
	local.SetOutput(os.Stdout)
	local.PrintDefaults()
}

// ParseSub parses subcommand arguments allowing flags and positionals to be
// interleaved (go flag stops at the first positional; usage is
// `wtm add <branch> [<start-point>] [-p|--path <dir>]`). -h prints the
// subcommand flags and then global usage, and yields errHelpOK.
// Globals are validated here after parsing: subFlags binds them to g, so a
// bogus --color/--progress placed after the subcommand is only visible now,
// not in dispatch's pre-parse check.
func ParseSub(fs *flag.FlagSet, g *Globals, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				PrintSubFlags(fs, g)
				fmt.Print(UsageText)
				return nil, ErrHelpOK
			}
			return nil, Usagef("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			if err := ValidateGlobals(g); err != nil {
				return nil, err
			}
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// ErrHelpOK is returned when -h was handled and usage was already printed.
var ErrHelpOK = errors.New("usage shown")

// ValidateGlobals rejects a bogus --color/--progress once for every subcommand.
func ValidateGlobals(g *Globals) error {
	if err := validateColor(g.Color); err != nil {
		return err
	}
	return validateProgress(g.Progress)
}

// validateColor rejects an unknown --color value so it can never silently
// fall back to auto.
func validateColor(mode string) error {
	switch mode {
	case "", ui.ColorAuto, ui.ColorAlways, ui.ColorNever:
		return nil
	}
	return Usagef("--color: want auto, always or never, got %q", mode)
}

// validateProgress rejects an unknown --progress value so it can never silently
// fall back to auto.
func validateProgress(mode string) error {
	switch mode {
	case "", ui.ProgressAuto, ui.ProgressAlways, ui.ProgressNever:
		return nil
	}
	return Usagef("--progress: want auto, always or never, got %q", mode)
}
