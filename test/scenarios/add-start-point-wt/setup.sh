#!/usr/bin/env bash
# Seed: main plus a linked worktree on base-branch, each carrying a distinct
# .chunkhound.json marker. `wtm add` must seed from the start-point's worktree,
# not from main.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt-base" base-branch main
printf '{"from":"base"}\n' >"$SBX/wt-base/.chunkhound.json"
printf '{"from":"main"}\n' >"$SBX/repo/.chunkhound.json"
