#!/usr/bin/env bash
# Seed: a local bare origin with the worktree's branch pushed and upstream
# configured, so delete can remove the remote branch offline (--network none).
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
printf 'hello\n' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

git -C "$SBX" init -q --bare -b main origin.git
git -C "$SBX/repo" remote add origin "$SBX/origin.git"
git -C "$SBX/repo" push -q origin main

worktree_add "$SBX/repo" "$SBX/dead" feature/remote
git -C "$SBX/repo" push -q origin feature/remote:feature/remote
git -C "$SBX/repo" branch --set-upstream-to=origin/feature/remote feature/remote
mk_session "$SBX/dead" "s-dead" "$FUTURE_STAMP"
