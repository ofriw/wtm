#!/usr/bin/env bash
# Seed: as gc-remote, but origin rejects deletes, so the remote delete fails and
# gc must exit non-zero with a clear error while still removing the worktree.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
printf 'hello\n' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

git -C "$SBX" init -q --bare -b main origin.git
git -C "$SBX/origin.git" config receive.denyDeletes true
git -C "$SBX/repo" remote add origin "$SBX/origin.git"
git -C "$SBX/repo" push -q origin main

worktree_add "$SBX/repo" "$SBX/wt1" wt1
git -C "$SBX/repo" push -q origin wt1:wt1
git -C "$SBX/repo" branch --set-upstream-to=origin/wt1 wt1
mk_session "$SBX/wt1" "s-wt1" "$OLD_STAMP"
mk_session "$SBX/repo" "s-main" "$FUTURE_STAMP"
backdate "$SBX/wt1/.git"
