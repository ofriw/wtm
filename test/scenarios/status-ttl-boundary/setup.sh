#!/usr/bin/env bash
# Seed: three linked worktrees whose creation baselines (.git mtimes) straddle
# the configured 10-day TTL: fresh (now) and edge (9 days) stay ACTIVE, old
# (11 days) is UNUSED. Touch .git last so no later write re-stamps it.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt-edge" edge
worktree_add "$SBX/repo" "$SBX/wt-old" old
worktree_add "$SBX/repo" "$SBX/wt-fresh" fresh

touch -d '9 days ago' "$SBX/wt-edge/.git"
touch -d '11 days ago' "$SBX/wt-old/.git"

wtm config set unusedTTL 10d
