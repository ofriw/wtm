# wtm

**A new git worktree, ready to work in — in one command. And the worktrees you've abandoned, reclaimed safely.**

`wtm` manages the worktree lifecycle of a [ChunkHound](https://chunkhound.ai) + [Pi.dev](https://pi.dev) + Claude Code software factory.

[![ci](https://github.com/ofriw/wtm/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ofriw/wtm/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)
![License](https://img.shields.io/badge/license-Unlicense-blue)

## The problem

A fresh `git worktree add` gives you checked-out files and nothing else — no agent harness, no search index — so every new checkout starts with the same manual setup. Meanwhile, the worktrees you're done with stay on disk forever, invisible until you run out of space or branch names collide.

## What it does

- **`add`** — create a worktree that's ready to work in: agent harness wired up, ChunkHound index seeded and refreshed. If the branch already exists, `add` attaches to it instead of creating a new one.
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

## Attaching an existing branch

`wtm add <branch>` attaches when the branch already exists locally. The checkout lands on that
branch, its configured upstream is preserved, and the same harness and index setup runs. A
branch that is already checked out in another worktree fails and names that worktree. A supplied
start-point must match the branch tip. `--temp` always requires a new branch.

Matching keeps Git's stored spelling. `wtm add` tries the raw name first, exact then as a unique
case-insensitive match. If neither matches, it tries the normalized name (lowercase, `_` and
spaces become `-`), again exact then case-insensitive. A request that matches several branches
only by case is refused as ambiguous; an exact spelling wins. Names that normalization would
rewrite or reject, such as `Feature/under_score` or `Feature/C++`, attach as written.

Use the branch name reported by `add`: `--json` sets `attached` to `true` or `false`, and human
output appends `(existing)`.

```console
$ wtm add feature/under_score --no-index
worktree: /Users/you/dev/app-feature-under-score
branch:   Feature/under_score (existing)
base:     refs/heads/Feature/under_score (local)
source:   /Users/you/dev/app
indexed:  false

$ wtm add feature/under_score --no-index --json
{
  "path": "/Users/you/dev/app-feature-under-score",
  "branch": "Feature/under_score",
  "base": "refs/heads/Feature/under_score",
  "source": "/Users/you/dev/app",
  "indexed": false,
  "attached": true,
  "temp": false
}
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

**Changed:** `wtm add` now seeds a new branch with no `--from` and no start-point from a usable worktree that has the default branch checked out, even when that worktree is ahead of or behind the resolved default ref. The new branch still forks from the resolved default ref. If several usable worktrees check out the default branch, the primary worktree seeds first; otherwise the linked worktree with the lowest path wins. Equal committed trees produce no notice. If no usable default-branch worktree exists, the source falls back to the worktree at the resolved ref, then the main worktree, and warns only when the committed trees differ. `--from` remains silent.

- Default-branch source with a different tree: `seeding from <path>: default branch <name> is checked out there, but the start-point is <ref>`.
- Create fallback with a different tree: `no usable worktree for <ref>; seeding from <path>`. A create fallback with equal committed trees no longer warns.
- Attach fallback: `branch <ref> is not checked out; seeding from <path>` (unchanged).
- `--no-index` with a copied database appends: `; the copied ChunkHound index is stale: indexing was skipped`.
- The old `the copied ChunkHound index cannot match and indexing will re-embed` suffix is removed. It appeared only when indexing ran over a copied database. `--no-index` did not emit it. Scripts that match it must change.

Copied worktree configs may hold API keys. `wtm` never commits them — add them to `.gitignore` if they do.

## Configuration

| Key | Default | Location |
|---|---|---|
| `unusedTTL` | `90d` | `~/.wtm/settings.json` |

`unusedTTL` is a positive whole number of days and defines the `ACTIVE` / `UNUSED` boundary.

## License

[Unlicense](LICENSE) — public domain.
