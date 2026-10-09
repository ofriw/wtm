#!/usr/bin/env bash
# Seed: main ACTIVE, wt1 UNUSED with a Claude session. With --keep-sessions the
# Claude project dir must survive the worktree removal.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt1" wt1
mk_claude_session "$SBX/wt1" "claude-sess" "$OLD_STAMP"
backdate "$SBX/wt1/.git"
