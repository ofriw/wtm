#!/usr/bin/env bash
# Seed: a plain permanent worktree on wt-perm and no temp store at all.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/repo-wt-perm" wt-perm
