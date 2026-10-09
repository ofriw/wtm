#!/usr/bin/env bash
# Same seed as delete-claude-purge; --keep-sessions must preserve the Claude
# transcript byte-for-byte. The golden manifest pins the survivor.
. "${HARNESS:-/harness}/lib.sh"
git_init "$SBX/repo" main
printf 'hello\n' >"$SBX/repo/README.md"
commit "$SBX/repo" seed README.md
worktree_add "$SBX/repo" "$SBX/wt1" feature/dead
mk_claude_session "$SBX/wt1" session-1 "$OLD_STAMP"
backdate "$SBX/wt1/.git"
