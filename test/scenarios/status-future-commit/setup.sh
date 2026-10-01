#!/usr/bin/env bash
# Seed: a worktree with a commit dated 2035. worktreeLastUsed clamps future
# timestamps to now, so the worktree reads ACTIVE today instead of pinning a
# 2035 LAST USED cell (or, without the clamp, drifting with the clock).
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt-future" fut
echo 'work' >"$SBX/wt-future/work.txt"
GIT_AUTHOR_DATE='2035-01-01T00:00:00+00:00' GIT_COMMITTER_DATE='2035-01-01T00:00:00+00:00' \
  git -C "$SBX/wt-future" add work.txt
GIT_AUTHOR_DATE='2035-01-01T00:00:00+00:00' GIT_COMMITTER_DATE='2035-01-01T00:00:00+00:00' \
  git -C "$SBX/wt-future" commit -q -m future

backdate "$SBX/wt-future/.git"
