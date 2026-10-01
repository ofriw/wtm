#!/usr/bin/env bash
# Seed: plain repo (main) with no chunkhound workspace at all. `wtm add` must
# create the worktree and index it there.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
