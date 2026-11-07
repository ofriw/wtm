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

# WTM_NOW pins the clock at 2026-01-01; the mtimes are absolute so the
# boundary is deterministic instead of drifting with the wall clock.
# touch_git_activity also stamps the private index, so staged-activity reads
# see the same instant as the checkout backlink.
touch_git_activity "$SBX/wt-edge" -d '2025-12-23T00:00:00Z'
touch_git_activity "$SBX/wt-old" -d '2025-12-21T00:00:00Z'

wtm config set unusedTTL 10d
