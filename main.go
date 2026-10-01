// wtm — worktree manager for the ChunkHound + Pi.dev software factory.
//
// One package, no sub-packages, no interfaces: external invariants are pinned
// by the container FS-diff harness under test/, not by Go unit tests.
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
  wtm add <path> [-b <branch> | --branch <branch>] [<start-point>] [--no-index] [--from <src>]
  wtm gc [--all | --path <p> ...] [--keep-sessions]
  wtm config [set unusedTTL <Nd>]

options:
  --root <dir>   repository root (default: main worktree of the current repo)
  --json         machine-readable output
  --color <when> color output: auto (default), always, never
  --yes          skip confirmation (gc); use --all or --path to select non-interactively

exit codes: 0 ok, 1 error, 2 usage
`

// errHelpOK is returned when -h was handled and usage was already printed.
var errHelpOK = errors.New("usage shown")

// globals are flags accepted both before and after the subcommand.
type globals struct {
	root  string
	json  bool
	color string
	yes   bool
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.root, "root", g.root, "repository root")
	fs.BoolVar(&g.json, "json", g.json, "machine-readable output")
	// Default is the current value: subFlags re-registers and must not clobber
	// a --color parsed before the subcommand.
	fs.StringVar(&g.color, "color", g.color, "color output: auto, always, never")
	fs.BoolVar(&g.yes, "yes", g.yes, "never prompt")
}

// usageError maps to exit code 2.
type usageError string

func (e usageError) Error() string { return string(e) }

func usagef(format string, a ...any) error { return usageError(fmt.Sprintf(format, a...)) }

func main() { os.Exit(run(os.Args[1:])) }

func dispatch(cmd string, g *globals, rest []string) error {
	// --color is global: validate once here so every subcommand (status, add,
	// gc, config) rejects a bogus value instead of only status enforcing it.
	if err := validateColor(g.color); err != nil {
		return err
	}
	switch cmd {
	case "status":
		return cmdStatus(g, rest)
	case "add":
		return cmdAdd(g, rest)
	case "gc":
		return cmdGC(g, rest)
	case "config":
		return cmdConfig(g, rest)
	case "help":
		fmt.Print(usage)
		return errHelpOK
	default:
		return usagef("unknown command %q", cmd)
	}
}

func run(args []string) int {
	g := &globals{color: colorAuto}
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
// `wtm add <path> -b <branch>`). -h prints usage and yields errHelpOK.
func parseSub(fs *flag.FlagSet, args []string) ([]string, error) {
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
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}
