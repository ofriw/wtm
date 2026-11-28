// wtm — worktree manager for the ChunkHound + Pi.dev + Claude Code software factory.
//
// This package is the thin entry point; the implementation lives in shallow
// internal packages (cli, core, harness, temp, git, ui, fs). Interfaces remain
// forbidden: the project composes concrete types at the composition root.
package main

import (
	"os"

	"wtm/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:])) }
