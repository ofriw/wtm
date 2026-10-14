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
PATH                            BRANCH                UPSTREAM               LAST USED   AGE   ACTIVE   COST          STATUS  INTEGRATIONS
~/dev/app                       main                  origin/main            2026-09-30  12d   8h 20m   $0.00/$41.20  ACTIVE  chunk mcp claude
~/dev/app-feature-billing-fix   feature/billing-fix   origin/feature/...     2026-09-28  6h    2h 14m   $4.10         ACTIVE  chunk mcp claude
~/dev/old-spike                 spike-search          -                      2026-03-02  8M    45m      $0.00         UNUSED  chunk~ mcp

$ wtm gc --all --yes
removed /Users/you/dev/old-spike
pruned
sessions purged: 1
```

## Commands

`add` · `delete` · `gc` · `status` · `config` — run **`wtm --help`** for usage, flags, and exit codes.

`INTEGRATIONS` lists what each worktree carries (`~` marks a partial setup, e.g. a ChunkHound config without a database). `wtm status --wide` expands one column per integration; `--json` reports the same states in an `integrations` map (`yes` / `partial` / `-` / `n/a`).

`AGE` is the wall-clock life of the worktree (the `.git` mtime is the creation mark); `ACTIVE` is the time agent sessions were in use (overlapping spans count once); `COST` is the actual agent charge. A worktree with no recorded usage shows `-` for `ACTIVE` and `COST`; `AGE` shows `-` only when the worktree has no creation mark. `COST` reads `unknown` when a recorded event carries no price, so a partial sum never looks complete. For the main worktree `.git` is a directory, so any index or ref write re-stamps it and `AGE` reads young again; linked worktrees keep a stable mark.

A subscription changes what "actual charge" means. When the current credentials prove an active subscription, `COST` renders the billed cost beside the pay-as-you-go list price (`$0.00/$41.20` above): the left side is what you actually pay, the right side is what the same tokens would cost metered, and `unknown` replaces either side when no price is available. A Claude account that enables extra usage — or a legacy account file with no `hasExtraUsageEnabled` field — renders the covered `$0.00` as `~$0.00` — an estimate, because usage can overflow the allowance and bill at list price. Without proof, `wtm` shows the list price alone — exactly as if no subscription existed. The proof rules:

- **Pi** — `openai-codex` authenticates only through OAuth, so a session naming it is covered. Every other subscription-backed provider (`anthropic`, `github-copilot`, `meta`, `openai`, `xai`) also accepts an API key, so its `auth.json` entry must say `oauth`. `kimi-coding` is always covered, mirroring Pi, even with an API key. Any other provider, or a missing or unreadable `auth.json`, is unproven. A proven `$0` is the subscription's marginal cost only — overage and other metered usage still bill.
- **Claude Code** — the account file must bill by `stripe_subscription`. wtm reads `<configDir>/.config.json` when it exists, else `<$CLAUDE_CONFIG_DIR or $HOME>/.claude[-custom-oauth].json` (the suffix when `CLAUDE_CODE_CUSTOM_OAUTH_URL` is set), where `<configDir>` is `$CLAUDE_CONFIG_DIR` or `~/.claude`. No metered override may be configured in the worktree, any parent directory, or the enterprise managed-settings file (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `apiKeyHelper`, Bedrock, Vertex, Foundry), and the session must not predate the subscription. When the subscription start date is missing or unparseable, dated events stay at list price — only a dateless event (no timestamp to contradict the proof) is treated as covered.

`WTM_CLAUDE_PARENT_SCOPE=0` skips the ancestor-settings walk; set it only when you know the harness never merges parent `.claude/settings*.json`. wtm warns once when it is active.

Because session files record no credential type, the dual-provider and Claude proofs read the *current* credentials; a past metered run under today's subscription credential reads as covered. A missing credential file proves nothing and never fails the command. Missing Claude settings files are harmless; malformed or unreadable settings invalidate the Claude subscription proof, so costs fall back to list price.

Pi prices its own sessions. Claude Code records tokens, not prices, so `wtm` derives its cost from the [LiteLLM public rate catalog](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json): the catalog is cached at `~/.wtm/rates.json` (24h TTL) and `WTM_RATES` pins a local copy for offline use. The first run on a Claude-only repo may wait briefly on the network; with no catalog and no cache, the Claude cost reads `unknown`. Test goldens pin a minimal subset of that catalog in `test/fixtures/rates.json`, so scenario runs never touch the network.

`WTM_NOW` (RFC3339) pins the clock; the container test suite sets it so worktree ages and the gc TTL boundary are byte-stable. It is a test seam, not a user setting.

Running `wtm status` below the table prints the current worktree's age, active time, and cost, then one row per (harness, model) with events.

The JSON output adds `current`, `ageSeconds` (null when unknown), `cost` (null when unknown or absent), and — for worktrees with recorded usage — `activeSeconds` and a `usage` map keyed by harness, each with `cost`, `activeSeconds`, and a per-`models` breakdown. `cost` is the billed amount; when a proven subscription makes it differ from the list price, a `listCost` key appears beside it at the worktree, harness, and model levels (`null` when the list is unknown, omitted entirely when no subscription is proven). Worktrees with no usage omit `activeSeconds` and `usage`.

**Breaking:** the `config`, `db`, `mcp`, and `mcpNative` boolean keys were removed. Update consumers to use the `integrations` string map instead (`wtm status --json | jq '.[].integrations'`). `mcp` keeps its old meaning — any recognized MCP config counts (`yes`); a harness's own config (`.pi/mcp.json`, `.claude/settings*.json`) also lights that harness's key. `chunk~` (and `partial` in `--wide`/JSON) is config-xor-database: exactly one side is present. `wtm` no longer reports which side — inspect the worktree to find out.

Copied worktree configs may hold API keys. `wtm` never commits them — add them to `.gitignore` if they do.

## Configuration

| Key | Default | Location |
|---|---|---|
| `unusedTTL` | `90d` | `~/.wtm/settings.json` |

`unusedTTL` is a positive whole number of days and defines the `ACTIVE` / `UNUSED` boundary.

## License

[Unlicense](LICENSE) — public domain.
