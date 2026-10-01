#!/usr/bin/env bash
# Seed: main and `other` diverge. the default start-point must be main, not the
# most recently touched branch.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

git -C "$SBX/repo" checkout -q -b other
echo 'other' >"$SBX/repo/other.txt"
commit "$SBX/repo" "other branch" other.txt
git -C "$SBX/repo" checkout -q main
