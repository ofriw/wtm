#!/usr/bin/env bash
# Pin backlink and private index activity across the 10-day TTL: fresh and
# edge (9 days) stay ACTIVE, old (11 days) is UNUSED.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt-edge" edge
worktree_add "$SBX/repo" "$SBX/wt-old" old
worktree_add "$SBX/repo" "$SBX/wt-fresh" fresh

touch_git_activity "$SBX/wt-edge" -d '9 days ago'
touch_git_activity "$SBX/wt-old" -d '11 days ago'

wtm config set unusedTTL 10d
