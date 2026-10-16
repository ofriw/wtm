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
PATH                           BRANCH               UPSTREAM            LAST USED   AGE  ACTIVE   COST    LIST  STATUS  INTEGRATIONS
~/dev/app                      main                 origin/main         2026-09-30  12d  8h 20m  $0.00  $41.20  ACTIVE  chunk mcp claude
~/dev/app-feature-billing-fix  feature/billing-fix  origin/feature/...  2026-09-28   6h  2h 14m  $4.10       -  ACTIVE  chunk mcp claude
~/dev/old-spike                spike-search         -                   2026-03-02   8M     45m  $0.00       -  UNUSED  chunk~ mcp

list = pay-as-you-go price of the same tokens

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

`AGE` is the wall-clock life of the worktree (the `.git` mtime is the creation mark); `ACTIVE` is the time agent sessions were in use (overlapping spans count once); `COST` is the actual agent charge, and `LIST` is what the same tokens would cost metered. All three time figures share one grammar: the non-zero units from years down to minutes (`2d 5h 11m`), with seconds only below a minute. A worktree with no recorded usage shows `-` for `ACTIVE`, `COST` and `LIST`; `AGE` shows `-` only when the worktree has no creation mark. `COST` reads `unknown` when a recorded event carries no price, so a partial sum never looks complete. For the main worktree `.git` is a directory, so any index or ref write re-stamps it and `AGE` reads young again; linked worktrees keep a stable mark.

A subscription changes what "actual charge" means. When the current credentials prove an active subscription, `wtm` splits the price into two columns: `COST` (`$0.00` above) is what you actually pay and `LIST` (`$41.20`) is what the same tokens would cost metered, each with its own right-aligned header. `LIST` appears only when some row carries a list price, so a table of single prices keeps its old shape, and a legend under the block that renders it repeats what the column holds. `unknown` replaces either value when no price is available. A Claude account that enables extra usage — or a legacy account file with no `hasExtraUsageEnabled` field — renders the covered `$0.00` as `~$0.00` — an estimate, because usage can overflow the allowance and bill at list price. Without proof, `wtm` shows the list price alone — exactly as if no subscription existed. The proof rules:

- **Pi** — `openai-codex` authenticates only through OAuth, so a session naming it is covered. Every other subscription-backed provider (`anthropic`, `github-copilot`, `meta`, `openai`, `xai`) also accepts an API key, so its `auth.json` entry must say `oauth`. `kimi-coding` is always covered, mirroring Pi, even with an API key. Any other provider, or a missing or unreadable `auth.json`, is unproven. A proven `$0` is the subscription's marginal cost only — overage and other metered usage still bill.
- **Claude Code** — the account file must bill by `stripe_subscription`. wtm reads `<configDir>/.config.json` when it exists, else `<$CLAUDE_CONFIG_DIR or $HOME>/.claude[-custom-oauth].json` (the suffix when `CLAUDE_CODE_CUSTOM_OAUTH_URL` is set), where `<configDir>` is `$CLAUDE_CONFIG_DIR` or `~/.claude`. No metered override may be configured in the worktree, any parent directory, or the enterprise managed-settings file (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `apiKeyHelper`, Bedrock, Vertex, Foundry), and the session must not predate the subscription. When the subscription start date is missing or unparseable, dated events stay at list price — only a dateless event (no timestamp to contradict the proof) is treated as covered.

`WTM_CLAUDE_PARENT_SCOPE=0` skips the ancestor-settings walk; set it only when you know the harness never merges parent `.claude/settings*.json`. wtm warns once when it is active.

Because session files record no credential type, the dual-provider and Claude proofs read the *current* credentials; a past metered run under today's subscription credential reads as covered. A missing credential file proves nothing and never fails the command. Missing Claude settings files are harmless; malformed or unreadable settings invalidate the Claude subscription proof, so costs fall back to list price.

Pi prices its own sessions. Claude Code records tokens, not prices, so `wtm` derives its cost from the [LiteLLM public rate catalog](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json): the catalog is cached at `~/.wtm/rates.json` (24h TTL) and `WTM_RATES` pins a local copy for offline use. The first run on a Claude-only repo may wait briefly on the network; with no catalog and no cache, the Claude cost reads `unknown`. Test goldens pin a minimal subset of that catalog in `test/fixtures/rates.json`, so scenario runs never touch the network.

`WTM_NOW` (RFC3339) pins the clock; the container test suite sets it so worktree ages and the gc TTL boundary are byte-stable. It is a test seam, not a user setting.

Running `wtm status` below the table prints the current worktree's age, active time, and cost, then — when the worktree recorded output — its input-to-output shares (`total in:out`, `cached in:out`, `uncached in:out`, all dividing the same recorded output, so they add up before rounding; displayed 1-decimal values can differ by 0.1), then one row per model with events (models with the same name in two harnesses merge to one row, summing cost, tokens, and active time; `--json` keeps the per-harness split). Each row shows the model's time, its billed `COST` and — for a proven subscription — the `LIST` price beside it, then the token group: `↑ in` (total prompt tokens — uncached input plus cache read plus cache write), `↓ out`, `↩ hit` (cache read), `↪ write` (cache write), `× miss` (uncached input plus cache write), and `● hit%` (`hit / in`, caps at 99.9%). `-` marks a class the model never recorded; `hit%` reads `unknown` when a hit has no recorded build to divide by. A legend under the rows repeats the counting. A narrow terminal hides the whole token group at once — never a partial breakdown — and the ratio group as a unit, each drop disclosed on stderr; `--json` always carries the numbers. Spawn-child costs carry no tokens, so a model's token columns can stay empty while its cost is nonzero. Web-search cost joins the total only, so tier costs can sum below the model total.

The JSON output adds `current`, `ageSeconds` (null when unknown), `cost` (null when unknown or absent), and — for worktrees with recorded usage — `activeSeconds` and a `usage` map keyed by harness, each with `cost`, `activeSeconds`, and a per-`models` breakdown. `cost` is the billed amount; when a proven subscription makes it differ from the list price, a `listCost` key appears beside it at the worktree, harness, model, and token-tier levels (`null` when the list is unknown, omitted entirely when no subscription is proven). A model with recorded tokens adds a `tiers` map (`input`, `cacheRead`, `cacheWrite`, `output`), each tier carrying its `tokens` count, `cost`, and optional `listCost`, plus `cacheMissTokens` (uncached input plus cache write) and `cacheHitRate` (`cacheRead / in`, caps at 0.999, where `in` holds all prompt tokens with unclassified writes, omitted when no build is recorded). A worktree with recorded output adds `inputOutputRatio` — `total`, `cached`, and `uncached` input-to-output shares that all divide the same recorded output, so they add up before rounding (`total = cached + uncached`); the key is omitted when no output was recorded, because a ratio without a denominator is undefined. Worktrees with no usage omit `activeSeconds` and `usage`.

**Breaking (earlier integrations change):** the `config`, `db`, `mcp`, and `mcpNative` boolean keys were removed when the integrations were unified — not by cost tracking. Update consumers to use the `integrations` string map instead (`wtm status --json | jq '.[].integrations'`). `mcp` keeps its old meaning — any recognized MCP config counts (`yes`); a harness's own config (`.pi/mcp.json`, `.claude/settings*.json`) also lights that harness's key. `chunk~` (and `partial` in `--wide`/JSON) is config-xor-database: exactly one side is present. `wtm` no longer reports which side — inspect the worktree to find out.

Copied worktree configs may hold API keys. `wtm` never commits them — add them to `.gitignore` if they do.

## Configuration

| Key | Default | Location |
|---|---|---|
| `unusedTTL` | `90d` | `~/.wtm/settings.json` |

`unusedTTL` is a positive whole number of days and defines the `ACTIVE` / `UNUSED` boundary.

## License

[Unlicense](LICENSE) — public domain.
