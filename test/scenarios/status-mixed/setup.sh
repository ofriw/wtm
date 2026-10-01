#!/usr/bin/env bash
# Seed: main (config+db+mcp, ACTIVE) plus linked (db, UNUSED), detached
# (config only) and locked (bare) worktrees. Pins the porcelain parser, the
# creation-baseline activity model, the ACTIVE/UNUSED cutoff and the sort order.
. "${HARNESS:-/harness}/lib.sh"

CONFIG='{"llm":{"provider":"openai","model":"gpt-4o-mini"}}'

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt-linked" linked
printf '%s\n' "$CONFIG" >"$SBX/wt-linked/.chunkhound.json"
db_index "$SBX/wt-linked"
mk_session "$SBX/wt-linked" "s-linked" "$OLD_STAMP"

git -C "$SBX/repo" worktree add --detach "$SBX/wt-detached" main
printf '%s\n' "$CONFIG" >"$SBX/wt-detached/.chunkhound.json"

git -C "$SBX/repo" worktree add --lock -b locked "$SBX/wt-locked" main

printf '%s\n' "$CONFIG" >"$SBX/repo/.chunkhound.json"
db_index "$SBX/repo"
printf '{"mcpServers":{"chunkhound":{"command":"chunkhound","args":["mcp"]}}}\n' >"$SBX/repo/.mcp.json"
mk_session "$SBX/repo" "s-main" "$FUTURE_STAMP"

# The activity model now reads the worktree's `.git` mtime as the creation
# baseline; backdate it on the old worktrees so their seeded 2020 sessions /
# commits — not the sandbox build time — decide ACTIVE vs UNUSED.
backdate "$SBX/wt-linked/.git" "$SBX/wt-detached/.git" "$SBX/wt-locked/.git"
