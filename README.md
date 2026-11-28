# wtm

**A Git worktree manager for AI-native software factories.** One command turns a bare checkout into a workplace that is ready — agent harness wired up, search index seeded — and prices everything it hosts.

[![ci](https://github.com/ofriw/wtm/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ofriw/wtm/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)
![License](https://img.shields.io/badge/license-Unlicense-blue)

## The problem

A fresh `git worktree add` gives you checked-out files. Nothing else. No agent harness, no search index, no record of what a branch has already spent. Every new checkout starts from zero.

Meanwhile, finished worktrees stay on disk forever — invisible until you run out of space or a branch name collides. And no one can say what a feature actually cost to build.

## What wtm is

`wtm` runs the floor of a software factory. One command makes a checkout ready to work in, `status` shows the whole floor at a glance, and `gc` reclaims what has been abandoned. It plays two roles:

**Butler.** Makes checkouts ready: harness wired up, index seeded, branch attached. Knows what exists, what is active, and what is abandoned. Reclaims the abandoned ones — safely, with every destructive step disclosed first. Scratch checkouts clean up after themselves.

**Accountant.** Prices each worktree: age, active time, and model spend. Without measurement, no part has a price. With it, planning runs on real numbers.

`wtm` is harness-agnostic. It serves Pi, Claude Code, ChunkHound, or plain MCP — and reports what each worktree carries instead of depending on it.

## What it does

| Command | What it does |
|---|---|
| `add` | Create a worktree, ready to work in. If the branch exists, attach to it. |
| `status` | See every worktree: active or unused, age, active time, cost, integrations. |
| `delete` | Reclaim a single worktree by branch — even an active one. |
| `gc` | Reclaim the unused ones, and nothing else. Every destructive step is disclosed first. |
| `promote` | Make a temporary worktree permanent. |
| `config` | One setting today: `unusedTTL`. |

## Where it fits

**Stacked PRs.** Give each tier of a stack its own worktree. `wtm add` attaches to any existing branch mid-stack; the double-checkout guard stops two worktrees from fighting over one branch. Want a scratch tier? `--temp` creates one, and `promote` makes it permanent when it earns its place. `status` shows which tiers went cold, so you know what to rebase and what to reclaim.

**Attaching to existing branches.** Pick up a teammate's branch, or revisit a tier of your own stack. `wtm add <branch>` attaches when the branch exists locally: the checkout lands on it, its upstream is preserved, and the same setup runs. Matching is forgiving but safe — raw name first, then normalized spelling, ambiguous matches refused, and a loud, helpful failure when the branch is checked out elsewhere. Full rules below.

## Install

Requires **git** and **Go 1.26+**. ChunkHound is optional — without it, `add` seeds the worktree and skips indexing.

```sh
git clone https://github.com/ofriw/wtm
cd wtm
go build -o wtm .
```

Put `wtm` on your `PATH`. It works from any repo; `--root` targets one explicitly.

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
PATH                            BRANCH                UPSTREAM               LAST USED   STATUS    INTEGRATIONS
~/dev/app                       main                  origin/main            2026-09-30  ACTIVE    chunk mcp claude
~/dev/app-feature-billing-fix   feature/billing-fix   origin/feature/...     2026-09-28  ACTIVE    chunk mcp claude
~/dev/old-spike                 spike-search          -                      2026-03-02  UNUSED    chunk~ mcp

$ wtm gc --all --yes
removed /Users/you/dev/old-spike
pruned
sessions purged: 1
```

## Cost & time tracking

Every part has a price. `wtm status` measures each worktree: **AGE**, **ACTIVE** time, and **COST** in model spend. The detail block breaks cost down per model. Subscription proof shows billed cost beside list price — e.g. `$0.00/$41.20` — so you can tell coverage from metered burn. Unknown costs stay unknown, never a silent zero, and `~` marks an estimate: a value from a partial total that an unreadable session or record may change.

**Proof.** A proven subscription splits `COST` (what you actually pay) from `LIST` (what the same tokens would cost metered), each with its own header. Pi proves coverage through OAuth: a session naming `openai-codex` is covered, and every other subscription-backed provider must store `oauth` in its `auth.json` (`kimi-coding` is always covered). Claude Code proves coverage through its account file: it must bill by one of the subscription plans, hold no metered override in the session's own settings, its parents, or the enterprise managed-settings file, and not predate the subscription. Without proof, `wtm` shows the list price alone.

**History caveat.** Session files record no credential type, so both proofs read the *current* credentials: a past metered run under today's subscription credential reads as covered and may display `$0.00`. A missing credential file proves nothing and never fails the command.

**Pricing.** Pi prices its own sessions. Claude Code records tokens, not prices, so `wtm` derives cost from the [LiteLLM public rate catalog](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json): the catalog is cached at `~/.wtm/rates.json` (24h TTL), and `WTM_RATES` pins a local copy for offline use. With no catalog and no cache, the Claude cost reads `unknown`.

**JSON contract.** `wtm status --json` adds `current`, `ageSeconds` (null when unknown), `cost` (null when unknown), and — for worktrees with recorded usage — `activeSeconds`, a per-harness `usage` map, and `partial:true` when a total lost an unreadable session. `costEstimated` marks an estimated billed cost; `listCost`/`listCostEstimated` appear only when a proven subscription makes the billed and list values differ. `inputOutputRatio` carries `total`/`cached`/`uncached` shares when output was recorded.

**Env seams.** `WTM_NOW` and `WTM_CREATED_AT` (RFC3339) pin the clock and creation marks; `WTM_RATES` pins the catalog; `WTM_MANAGED_SETTINGS` replaces the Claude managed-settings file list; `WTM_CLAUDE_PARENT_SCOPE=0` skips the ancestor-settings walk; `WTM_PROGRESS_DELAY` pins the silence window; `WTM_USAGE_BYTE_BUDGET` caps the session bytes one status run reads (default 1 GiB; `0` disables the cap; over-budget sessions are skipped and totals read partial); `WTM_STATUS_SKIP_USAGE=1` skips usage loading entirely. None are user settings — they exist for tests and offline pinning.

**Interrupts.** A SIGINT/SIGTERM aborts a run and exits 1, not 0, so an interrupted run is never mistaken for success.

## Temporary worktrees

For short-lived work — PR reviews, repros, experiments — `wtm add <name> --temp` creates a checkout on a `tmp/<name>` branch that `gc` reclaims once it has been idle longer than its window (`--ttl`, default `1h`). Activity pushes the deadline forward, so the window measures idleness, not age.

## Attaching an existing branch

`wtm add <branch>` attaches when the branch already exists locally. The checkout lands on that branch, its configured upstream is preserved, and the same harness and index setup runs. A branch already checked out in another worktree fails and names that worktree. A supplied start-point must match the branch tip. `--temp` always requires a new branch.

Matching keeps Git's stored spelling. `wtm add` tries the raw name first, exact then as a unique case-insensitive match. If neither matches, it tries the normalized name (lowercase; `_` and spaces become `-`), again exact then case-insensitive. A request that matches several branches only by case is refused as ambiguous; an exact spelling wins. Names that normalization would rewrite or reject — `Feature/under_score`, `Feature/C++` — attach as written.

## Commands

`add` · `promote` · `delete` · `gc` · `status` · `config` — run **`wtm --help`** for usage, flags, and exit codes.

`INTEGRATIONS` lists what each worktree carries (`~` marks a partial setup, e.g. a ChunkHound config without a database). `wtm status --wide` expands one column per integration; `--json` reports the same states in an `integrations` map (`yes` / `partial` / `-` / `n/a`).

Copied worktree configs may hold API keys. `wtm` never commits them — add them to `.gitignore` if they do.

## Configuration

| Key | Default | Location |
|---|---|---|
| `unusedTTL` | `90d` | `~/.wtm/settings.json` |

`unusedTTL` is a positive whole number of days and defines the `ACTIVE` / `UNUSED` boundary.

## License

[Unlicense](LICENSE) — public domain.
