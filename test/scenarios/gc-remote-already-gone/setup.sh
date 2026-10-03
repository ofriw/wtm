#!/usr/bin/env bash
# Seed: a local bare origin where wt1's upstream branch has already been deleted
# out of band, leaving the tracking ref and upstream config behind. gc must treat
# the already-gone remote branch as a converged no-op, not a failure.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
printf 'hello\n' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

git -C "$SBX" init -q --bare -b main origin.git
git -C "$SBX/repo" remote add origin "$SBX/origin.git"
git -C "$SBX/repo" push -q origin main

worktree_add "$SBX/repo" "$SBX/wt1" wt1
git -C "$SBX/repo" push -q origin wt1:wt1
git -C "$SBX/repo" branch --set-upstream-to=origin/wt1 wt1
# The remote branch disappears while wt1's upstream stays configured.
git -C "$SBX/origin.git" update-ref -d refs/heads/wt1
mk_session "$SBX/wt1" "s-wt1" "$OLD_STAMP"
mk_session "$SBX/repo" "s-main" "$FUTURE_STAMP"
backdate "$SBX/wt1/.git"
