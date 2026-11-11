#!/usr/bin/env bash
# Seed: the default branch is checked out in a linked worktree and has moved
# past origin/main. The primary checkout stays on "work" at origin/main, so a
# commit-only source lookup would silently seed the central path instead.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" work
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

git -C "$SBX/repo" branch main
git -C "$SBX/repo" worktree add -q "$SBX/wt-main" main
git -C "$SBX/repo" update-ref refs/remotes/origin/main refs/heads/main

printf '{"from":"main-linked"}\n' >"$SBX/wt-main/.chunkhound.json"
printf '{"from":"central"}\n' >"$SBX/repo/.chunkhound.json"
echo 'local' >"$SBX/wt-main/local.txt"
commit "$SBX/wt-main" "local main" local.txt
