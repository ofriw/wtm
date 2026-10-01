#!/usr/bin/env bash
# Seed: main plus a dirty (uncommitted tracked change) worktree. Backdate .git
# and the dirty file so it is UNUSED and the file's mtime is not activity.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/dirty" dirty
echo 'dirty-edit' >>"$SBX/dirty/README.md"
backdate "$SBX/dirty/.git" "$SBX/dirty/README.md"
