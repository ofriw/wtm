// wtm — worktree manager for the ChunkHound + Pi.dev + Claude Code software factory.
//
// One package, no sub-packages, no interfaces: the container FS-diff harness
// under test/ is authoritative for integration, while Go unit tests pin the
// pure contracts (inventory, JSON schema, width policy).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const usage = `wtm — worktree manager

usage:
  wtm [options]                    status of all worktrees (default command)
  wtm status --wide                one column per integration, not the grouped cell
  wtm add <branch> [<start-point>] [-p|--path <dir>] [--no-index] [--from <src>] [--temp] [--ttl <dur>]
  wtm promote <branch>
  wtm delete <branch> [--keep-remote] [--keep-sessions] [--yes]
  wtm gc [--all | --path <p> ...] [--keep-sessions] [--keep-remote]
  wtm config [set unusedTTL <Nd>]

options:
  --root <dir>      repository root (default: main worktree of the current repo)
  --json            machine-readable output
  --color <when>    color output: auto (default), always, never
  --progress <when> progress display: auto (default), always, never
  --yes             skip confirmation (gc, delete); use --all or --path to select non-interactively

exit codes: 0 ok, 1 error, 2 usage
`

// errHelpOK is returned when -h was handled and usage was already printed.
var errHelpOK = errors.New("usage shown")

// globals are flags accepted both before and after the subcommand.
type globals struct {
	root     string
	json     bool
	color    string
	progress string
	yes      bool
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.root, "root", g.root, "repository root")
	fs.BoolVar(&g.json, "json", g.json, "machine-readable output")
	// Default is the current value: subFlags re-registers and must not clobber
	// a --color/--progress parsed before the subcommand.
	fs.StringVar(&g.color, "color", g.color, "color output: auto, always, never")
	fs.StringVar(&g.progress, "progress", g.progress, "progress display: auto, always, never")
	fs.BoolVar(&g.yes, "yes", g.yes, "never prompt")
}

// usageError maps to exit code 2.
type usageError string

func (e usageError) Error() string { return string(e) }

func usagef(format string, a ...any) error { return usageError(fmt.Sprintf(format, a...)) }

func main() { os.Exit(run(os.Args[1:])) }

func validateGlobals(g *globals) error {
	if err := validateColor(g.color); err != nil {
		return err
	}
	return validateProgress(g.progress)
}

func dispatchSubcommand(cmd string, g *globals, rest []string) error {
	switch cmd {
	case "status":
		return cmdStatus(g, rest)
	case "add":
		return cmdAdd(g, rest)
	case "promote":
		return cmdPromote(g, rest)
	case "delete":
		return cmdDelete(g, rest)
	case "gc":
		return cmdGC(g, rest)
	case "config":
		return cmdConfig(g, rest)
	case "help":
		fmt.Print(usage)
		return errHelpOK
	}
	return usagef("unknown command %q", cmd)
}

func dispatch(cmd string, g *globals, rest []string) error {
	// Globals are validated once here so every subcommand rejects bogus values.
	if err := validateGlobals(g); err != nil {
		return err
	}
	return dispatchSubcommand(cmd, g, rest)
}

func run(args []string) int {
	g := &globals{color: colorAuto, progress: progressAuto}
	fs := flag.NewFlagSet("wtm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(usage)
			return 0
		}
		return fail(usagef("%v", err))
	}
	cmd, rest := "status", fs.Args()
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}
	return fail(dispatch(cmd, g, rest))
}

// fail maps an error to the exit contract: usage → 2, anything else → 1.
func fail(err error) int {
	switch {
	case err == nil, errors.Is(err, errHelpOK):
		return 0
	case errors.Is(err, errAborted):
		// "aborted" is already on stderr; exit 0, no extra output.
		return 0
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(os.Stderr, "wtm:", err)
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	fmt.Fprintln(os.Stderr, "wtm:", err)
	return 1
}

// subFlags builds a flag set for a subcommand; globals are re-registered so
// they may appear after the subcommand too (wtm status --json).
func subFlags(name string, g *globals) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	return fs
}

// parseSub parses subcommand arguments allowing flags and positionals to be
// interleaved (go flag stops at the first positional; usage is
// `wtm add <branch> [<start-point>] [-p|--path <dir>]`). -h prints usage and yields errHelpOK.
// Globals are validated here after parsing: subFlags binds them to g, so a
// bogus --color/--progress placed after the subcommand is only visible now,
// not in dispatch's pre-parse check.
func parseSub(fs *flag.FlagSet, g *globals, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
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
