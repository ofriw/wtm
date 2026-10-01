#!/usr/bin/env bash
# Seed: main ACTIVE, wt1 UNUSED with a session and an ignored untracked file
# (ignored files must not block `git worktree remove`), wt2 ACTIVE — the --all
# pass must skip it.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
printf '.env.local\n' >"$SBX/repo/.gitignore"
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" .gitignore README.md

worktree_add "$SBX/repo" "$SBX/wt1" wt1
echo 'junk' >"$SBX/wt1/.env.local"
mk_session "$SBX/wt1" "s-wt1" "$OLD_STAMP"

worktree_add "$SBX/repo" "$SBX/wt2" wt2
mk_session "$SBX/wt2" "s-wt2" "$FUTURE_STAMP"

mk_session "$SBX/repo" "s-main" "$FUTURE_STAMP"

# wt1 must pass the activity cutoff, so backdate its creation baseline. Its
# only extra file (.env.local) is git-ignored and therefore not activity.
backdate "$SBX/wt1/.git"
