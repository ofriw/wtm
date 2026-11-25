package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"wtm/internal/core"
	"wtm/internal/harness"
	"wtm/internal/ui"
)

// cli.go — the command layer: dispatch, exit-code mapping and the harness
// orchestration (session discovery hooks, temp activity injection). The flag
// framework lives here (flags.go); core receives a neutral Options struct, so
// the domain layer owns no CLI semantics. Handlers use the local globals and
// core types directly: no type aliases.

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

// Run is the CLI entry point; root's main() calls it.
func Run(args []string) int {
	harness.RegisterCapabilities() // composition root: core panics without it
	g := &globals{Color: ui.ColorAuto, Progress: ui.ProgressAuto}
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

func dispatch(cmd string, g *globals, rest []string) error {
	// globals are validated once here so every subcommand rejects bogus values.
	if err := validateGlobals(g); err != nil {
		return err
	}
	return dispatchSubcommand(cmd, g, rest)
}

func dispatchSubcommand(cmd string, g *globals, rest []string) error {
	switch cmd {
	case "status":
		return cmdStatus(g, rest, defaultHooks())
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

// fail maps an error to the exit contract: usage → 2, anything else → 1.
func fail(err error) int {
	switch {
	case err == nil, errors.Is(err, errHelpOK):
		return 0
	case errors.Is(err, errAborted):
		// "aborted" is already on stderr; exit 0, no extra output.
		return 0
	}
	var ue core.UsageError
	if errors.As(err, &ue) {
		fmt.Fprintln(os.Stderr, "wtm:", err)
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	fmt.Fprintln(os.Stderr, "wtm:", err)
	return 1
}

// defaultHooks is the shared status wiring used by dispatch and by cli tests,
// so the tests exercise exactly what the real dispatcher passes to core.
func defaultHooks() core.Hooks {
	return core.Hooks{
		Sessions: harness.DiscoverySessions(false),
		MCP:      harness.MCPConfigs,
	}
}

func loadWorkspace(g *globals, showProgress bool, sessions core.SessionDiscovery) (core.Workspace, error) {
	hooks := defaultHooks()
	hooks.Sessions = sessions
	return core.LoadWorkspace(g.opts(), showProgress, hooks)
}

// loadGCWorkspace selects the session policy: explicit --path removal stays
// lenient about ownership but strict about structured session errors; the
// default idle path uses the strict fresh index.
func loadGCWorkspace(g *globals, paths []string) (core.Workspace, error) {
	if len(paths) > 0 {
		return loadWorkspace(g, true, harness.DiscoverySessions(true))
	}
	return loadWorkspace(g, true, harness.IndexSessionsStrict)
}

// withProgress runs work against the live renderer when gating allows.
func withProgress(g *globals, enabled bool, work func(*ui.Progress) error) error {
	return withProgressTo(os.Stderr, g, enabled, work)
}

// withProgressTo is withProgress with an injectable renderer output, so tests
// can drive the renderer against a pipe instead of a terminal. The gating's
// TTY check still reads the real stderr: an explicit --progress always is what
// lets a piped output reach the renderer at all.
func withProgressTo(out io.Writer, g *globals, enabled bool, work func(*ui.Progress) error) error {
	return ui.RunMaybeProgress(out, g.Progress, g.JSON, enabled, work)
}
