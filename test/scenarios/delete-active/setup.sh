#!/usr/bin/env bash
# Seed: a linked worktree carrying a fresh session, so it reads ACTIVE with no
# remote upstream. delete must reclaim it anyway: the branch name is the
# consent, so the unused TTL does not gate it.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/dead" feature/dead
mk_session "$SBX/dead" "s-dead" "$FUTURE_STAMP"
