# wtm

**A new git worktree, ready to work in — in one command. And the worktrees you've abandoned, reclaimed safely.**

`wtm` manages the worktree lifecycle of a [ChunkHound](https://chunkhound.ai) + [Pi.dev](https://pi.dev) + Claude Code software factory.

[![ci](https://github.com/ofriw/wtm/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ofriw/wtm/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)
![License](https://img.shields.io/badge/license-Unlicense-blue)

## The problem

A fresh `git worktree add` gives you checked-out files and nothing else — no agent harness, no search index — so every new checkout starts with the same manual setup. Meanwhile, the worktrees you're done with stay on disk forever, invisible until you run out of space or branch names collide.

## What it does

- **`add`** — create a worktree that's ready to work in: agent harness wired up, ChunkHound index seeded and refreshed.
- **`status`** — see every worktree at a glance, and whether it's `ACTIVE` or `UNUSED`.
- **`delete`** — reclaim a single worktree by branch, even if it's `ACTIVE`.
- **`gc`** — reclaim the `UNUSED` ones, and nothing else, with every destructive step disclosed first.
- **`add --temp` / `promote`** — `add --temp` makes a throwaway checkout on a `tmp/` branch for collection by `gc`; `promote` makes it permanent.

## Install

Requires **git**, **Go 1.26+** to build (CI covers macOS, Linux, and Windows), and optionally **ChunkHound** — without it, `add` still seeds the worktree and simply skips indexing.

```sh
git clone https://github.com/ofriw/wtm
cd wtm
go build -o wtm .
```

Put `wtm` on your `PATH`; it works from any repo. `--root` targets one explicitly.

## Quick start

```console
$ cd ~/dev/app
$ wtm add feature/billing-fix
worktree: /Users/you/dev/app-feature-billing-fix
branch:   feature/billing-fix
base:     refs/remotes/origin/main (remote)
source:   /Users/you/dev/app
indexed:  true

$ wtm                      # bare `wtm` is `wtm status`
PATH                            BRANCH                UPSTREAM               LAST USED   STATUS  INTEGRATIONS
~/dev/app                       main                  origin/main            2026-09-30  ACTIVE  chunk mcp claude
~/dev/app-feature-billing-fix   feature/billing-fix   origin/feature/...     2026-09-28  ACTIVE  chunk mcp claude
~/dev/old-spike                 spike-search          -                      2026-03-02  UNUSED  chunk~ mcp

$ wtm gc --all --yes
removed /Users/you/dev/old-spike
pruned
sessions purged: 1
```

## Temporary worktrees

For short-lived tasks — PR reviews, repros, throwaway experiments — `wtm add <name> --temp`
creates a checkout on a `tmp/<name>` branch that `wtm gc` reclaims once it has been idle longer
than its window (`--ttl`, default `1h`). Activity pushes the deadline forward, so the window
measures idleness, not age. `wtm promote <branch>` makes a temp worktree permanent, and
`wtm status` shows a `TEMP` countdown column — always present, `-` for permanent checkouts.

See [temp-worktrees.md](temp-worktrees.md) for the full contract: registry and identity,
reclamation and safety rules, and promotion.

## Commands

`add` · `promote` · `delete` · `gc` · `status` · `config` — run **`wtm --help`** for usage, flags, and exit codes.

`INTEGRATIONS` lists what each worktree carries (`~` marks a partial setup, e.g. a ChunkHound config without a database). `wtm status --wide` expands one column per integration; `--json` reports the same states in an `integrations` map (`yes` / `partial` / `-` / `n/a`).

**Breaking:** the `config`, `db`, `mcp`, and `mcpNative` boolean keys were removed. Update consumers to use the `integrations` string map instead (`wtm status --json | jq '.[].integrations'`). `mcp` keeps its old meaning — any recognized MCP config counts (`yes`); a harness's own config (`.pi/mcp.json`, `.claude/settings*.json`) also lights that harness's key. `chunk~` (and `partial` in `--wide`/JSON) is config-xor-database: exactly one side is present. `wtm` no longer reports which side — inspect the worktree to find out.

Copied worktree configs may hold API keys. `wtm` never commits them — add them to `.gitignore` if they do.

## Configuration

| Key | Default | Location |
|---|---|---|
| `unusedTTL` | `90d` | `~/.wtm/settings.json` |

`unusedTTL` is a positive whole number of days and defines the `ACTIVE` / `UNUSED` boundary.

## License

[Unlicense](LICENSE) — public domain.
