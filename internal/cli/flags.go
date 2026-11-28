package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"wtm/internal/core"
	"wtm/internal/ui"
)

// flags.go — the command framework shared by every subcommand: the global flag
// state, usage-error mapping and the flag helpers. The CLI owns these; core
// receives a neutral Options struct so the domain layer carries no CLI
// semantics and never imports the flag machinery.

// usagef builds a core.UsageError.
func usagef(format string, a ...any) error { return core.UsageError(fmt.Sprintf(format, a...)) }

// globals are flags accepted both before and after the subcommand.
type globals struct {
	Root     string
	JSON     bool
	Color    string
	Progress string
	Yes      bool
	Flags    map[string]bool // names registered here; subcommand help hides them
}

// opts projects the globals onto core's neutral discovery inputs.
func (g *globals) opts() core.Options {
	return core.Options{Root: g.Root, JSON: g.JSON, Progress: g.Progress}
}

func (g *globals) register(fs *flag.FlagSet) {
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

// subFlags builds a flag set for a subcommand; globals are re-registered so
// they may appear after the subcommand too (wtm status --json).
func subFlags(name string, g *globals) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	return fs
}

// printSubFlags shows a subcommand's own flag descriptions. The global block in
// usage is their single printed home, so globals are filtered out here.
func printSubFlags(fs *flag.FlagSet, g *globals) {
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

// parseSub parses subcommand arguments, allowing flags and positionals to be
// interleaved (go flag stops at the first positional). -h prints the
// subcommand flags followed by global usage and yields errHelpOK; usage is the
// full help text passed per invocation by cli.Run, so -h shows the current
// command's text without a package-global. globals are validated here after
// parsing: subFlags binds them to g, so a bogus --color/--progress after the
// subcommand is only visible now, not in dispatch's pre-parse check.
func parseSub(fs *flag.FlagSet, g *globals, args []string, usage string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				printSubFlags(fs, g)
				fmt.Print(usage)
				return nil, errHelpOK
			}
			return nil, usagef("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			if err := validateGlobals(g); err != nil {
				return nil, err
			}
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// errHelpOK is returned when -h was handled and usage was already printed.
var errHelpOK = errors.New("usage shown")

// validateGlobals rejects a bogus --color/--progress once for every subcommand.
func validateGlobals(g *globals) error {
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
	return usagef("--color: want auto, always or never, got %q", mode)
}

// validateProgress rejects an unknown --progress value so it can never silently
// fall back to auto.
func validateProgress(mode string) error {
	switch mode {
	case "", ui.ProgressAuto, ui.ProgressAlways, ui.ProgressNever:
		return nil
	}
	return usagef("--progress: want auto, always or never, got %q", mode)
}
