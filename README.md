# wtm

**One command makes a new git worktree ready to work in; one command safely reclaims the ones you've abandoned.** `wtm` automates the worktree lifecycle of a [ChunkHound](https://chunkhound.ai) + [Pi.dev](https://pi.dev) software factory.

[![ci](https://github.com/ofriw/wtm/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ofriw/wtm/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)
![License](https://img.shields.io/badge/license-Unlicense-blue)

## Why it exists

A fresh `git worktree add` gives you checked-out files and nothing else — no agent harness, no search index — so every new checkout begins with the same manual setup. Meanwhile, the worktrees you're done with stay on disk forever, invisible until you run out of space or branch names collide.

`wtm` closes both gaps. `add` makes a new worktree productive in one step, `delete` reclaims one by branch, and `gc` turns "which of these can I delete?" into a safe, reviewable decision.

## Install

Requires:

- **git** — worktree operations
- **Go 1.26+** — to build; CI covers macOS, Linux, and Windows
- **ChunkHound** *(optional)* — `wtm` works without it. When it isn't installed, `add` still seeds the worktree and simply skips indexing.

```sh
git clone https://github.com/ofriw/wtm
cd wtm
go build -o wtm .
```

Put `wtm` on your `PATH`. It works from any repo, and `--root` targets one explicitly.

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
PATH                            BRANCH                UPSTREAM               LAST USED   STATUS  CONFIG  DB   MCP
~/dev/app                       main                  origin/main            2026-09-30  ACTIVE  yes     yes  yes
~/dev/app-feature-billing-fix   feature/billing-fix   origin/feature/...     2026-09-28  ACTIVE  yes     yes  yes
~/dev/old-spike                 spike-search          -                      2026-03-02  UNUSED  yes     -    yes

$ wtm gc --all --yes
removed /Users/you/dev/old-spike
pruned
sessions purged: 1
```

## Migrating

- `add` is branch-first: `wtm add <path> -b <branch>` → `wtm add <branch> [-p <dir>] [<start-point>]`. The first positional is always the branch; `-p`/`--path` is the only directory override.
- `wtm add ../foo` fails fast: branch input goes through `normalizeBranch` (lowercase, hyphen-normalized, must be a valid git ref), so path-like input is rejected instead of creating a stray directory.
- `status` gained an `UPSTREAM` column (`-` when untracked) and JSON keys `upstream`, `mcpNative`; `gc`/`delete --json` report `remoteDeleted`, `deletedBranch` (`delete` only), and `keptRemote`.
- Default-branch guard over-protects by design: on remotes with no locally recorded HEAD, `gc`/`delete` refuse any branch matching the repo default name on any remote — a coincidental name match is kept rather than risk deleting a shared default.

## Commands

### `wtm add <branch> [<start-point>]`

Creates a worktree that's immediately ready to work in: it wires up the Pi harness, carries over the ChunkHound workspace from the worktree at the start-point, points that workspace at the new worktree, and re-indexes when ChunkHound is available.

The branch is the only required input. The checkout directory is derived as `<repo>-<branch-slug>` and placed **next to the worktree you run `wtm` from** — never under the current directory — so worktrees cannot nest. A branch like `feature/login` keeps its taxonomy prefix while the directory flattens to `repo-feature-login`.

| Flag | Default | Meaning |
|---|---|---|
| `-p`, `--path <dir>` | sibling `<repo>-<branch-slug>` | explicit checkout directory; refused if it lands inside another worktree |
| `--from <src>` | worktree at the start-point | source worktree to seed from; overrides the start-point match |
| `--no-index` | off | skip indexing after seeding (also skipped automatically when ChunkHound isn't installed) |

- Branch names are normalized (lowercase, hyphens, `/` kept as a taxonomy separator) and validated as git refs. A branch without a conventional type prefix (`feature/`, `fix/`, `hotfix/`, …) warns but proceeds.
- `--path` is guarded: a directory inside (or equal to) any existing worktree is rejected, so a worktree never nests inside another.

- A missing `chunkhound` is not an error: `add` reports `indexed: false`, warning when the project has a ChunkHound workspace that would otherwise have been refreshed.
- Indexing is bounded to 5 minutes; a hung indexer fails with a timeout error rather than blocking `add` forever.
- If seeding fails, a pristine worktree is rolled back; a partially seeded one is kept and reported so you can inspect it.
- `<start-point>` defaults to the remote-tracking default branch (kept fresh for clones), falling back to a local `main` / `master` / `develop` / `trunk`, then `init.defaultBranch`, then the main worktree's branch.
- Seeding follows the start-point: `add` copies the harness — the gitignored db/pi/mcp state git cannot carry — from the worktree currently checked out at `<start-point>`, matched by commit so a diverged branch of the same name is never mistaken for it. When the start-point is checked out in no worktree, no matching index exists, so `add` falls back to the main worktree and warns that indexing will re-embed instead of being a no-op.
- MCP and ChunkHound configs commonly hold API keys. `wtm` copies recognized MCP configs (`.mcp.json`, `.pi/mcp.json`) and the ChunkHound workspace between worktrees locally and never commits them. It copies them even when your repo does not ignore them, so add them to `.gitignore` if they hold secrets.

### `wtm delete <branch>`

Reclaims a single worktree by its branch, even if `ACTIVE` — naming the branch is the consent, so the unused TTL does not apply. It runs the same destructive path as `gc`: the checkout is removed, the remote upstream is deleted (unless `--keep-remote`), Pi session history is purged (unless `--keep-sessions`), then `git worktree prune` runs and the local branch is force-deleted.

| Flag | Meaning |
|---|---|
| `--keep-remote` | keep the branch's remote upstream |
| `--keep-sessions` | keep Pi session history |
| `--yes` | skip confirmation (required without a terminal) |

```sh
wtm delete feature/billing-fix
wtm delete feature/billing-fix --yes --keep-remote
```

- The main worktree is never removable.
- The command is addressed by branch, so the local branch is removed too — unlike `gc`, which reclaims checkouts and leaves branches in place.
- A branch with **no** worktree (for example left behind by a failed `--keep-remote`-less delete, or never checked out) is still removable: `delete` then removes just the local branch and its remote upstream, under the same confirmation.
- Confirmation is required unless `--yes`.

### `wtm status`

The default command. Lists every worktree in the repo and whether its harness is intact.

| Column | Meaning |
|---|---|
| `PATH` | worktree path (`~`-shortened) |
| `BRANCH` | checked-out branch, `(detached)` if none |
| `UPSTREAM` | configured tracking branch, `-` if none |
| `LAST USED` | most recent of Pi session activity, `.git` change, commit, or edited/new file; `never` if none |
| `STATUS` | `ACTIVE` or `UNUSED` by `unusedTTL` |
| `CONFIG` · `DB` · `MCP` | whether the ChunkHound config, DB, and MCP config are present |

A worktree becomes `UNUSED` once `LAST USED` is older than `unusedTTL` (90 days by default).

The `MCP` column is `yes` when any recognized MCP config (`.mcp.json` or `.pi/mcp.json`) is present. With `--json`, each worktree additionally reports `upstream` (the configured tracking branch) and `mcpNative` (whether `.pi/mcp.json` specifically is present).

### `wtm gc`

Reclaims `UNUSED` worktrees — and nothing else. Removing the wrong checkout is expensive, so selection is always explicit and every destructive step is disclosed before it happens.

```sh
wtm gc                      # interactive multi-select, then confirm
wtm gc --all --yes          # every UNUSED worktree, no prompt
wtm gc --path ../old-spike  # one worktree (repeatable)
```

| Flag | Meaning |
|---|---|
| `--all` | select every UNUSED worktree |
| `--path <p>` | select a specific worktree (repeatable) |
| `--keep-remote` | keep the branch's remote upstream |
| `--keep-sessions` | keep Pi session history |
| `--yes` | skip confirmation (required without a terminal) |

- The **main worktree is never a candidate** — removing it would destroy the repo.
- Nothing is removed without explicit selection: interactive multi-select by default, `--all` / `--path` as opt-ins. Declining the prompt exits `0` with `aborted` on stderr.
- Each removal force-deletes the directory and, by default, the remote upstream branch and the Pi session history. `--keep-remote` and `--keep-sessions` opt out. On a removal, `--keep-remote` reports an actual keep (`keptRemote`, `remote branches kept`): with no remote upstreams it keeps nothing and reports accordingly. An aborted or empty `--all`/`--path` run reports the flag instead, since nothing was deleted.
- Remote deletion reaches the remote (`git push --delete`), so it requires the remote to be reachable. An upstream branch that is already gone is a no-op, and the remote's default branch is never deleted. Any other remote-delete failure is reported and makes `gc` exit `1`, even though the local worktree was already removed.
- Only the remote upstream branch is deleted; the worktree's local branch survives and keeps its (now dangling) upstream config. The "still tracked" veto considers only checked-out worktrees.
- `--path` on an `ACTIVE` worktree warns but proceeds — the path *is* the consent — and uncommitted changes are reported before being destroyed. There is no dry-run.

### `wtm config`

```console
$ wtm config
path: /Users/you/.wtm/settings.json
unusedTTL: 90d

$ wtm config set unusedTTL 30d
```

## Global flags

| Flag | Default | Meaning |
|---|---|---|
| `--root <dir>` | main worktree of the current repo | repository to operate on |
| `--json` | off | machine-readable output |
| `--color <when>` | `auto` | `auto`, `always`, or `never` |
| `--progress <when>` | `auto` | progress display: `auto`, `always`, or `never` |
| `--yes` | off | skip `gc` / `delete` confirmation |

Global flags work before or after the subcommand. Exit codes: `0` ok, `1` error, `2` usage.

## Configuration

| Key | Default | Location |
|---|---|---|
| `unusedTTL` | `90d` | `~/.wtm/settings.json` |

`unusedTTL` is positive whole days (`30d`, `90d`) and defines the `ACTIVE` / `UNUSED` boundary.

## License

[Unlicense](LICENSE) — public domain.
