#!/usr/bin/env bash
# Seed: main plus a linked worktree whose branch tracks a real origin branch, so
# the UPSTREAM column renders a value other than "-". Both worktrees are UNUSED
# and carry a fixed 2020 session, keeping the table deterministic.
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
mk_session "$SBX/repo" "s-main" "$OLD_STAMP"
mk_session "$SBX/wt1" "s-wt1" "$OLD_STAMP"
backdate "$SBX/repo/.git" "$SBX/wt1/.git"
