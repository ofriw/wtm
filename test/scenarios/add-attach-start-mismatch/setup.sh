#!/usr/bin/env bash
# Seed: feature/attach sits at the first commit while main has advanced, so a
# start-point of main contradicts the existing branch tip.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'one' >"$SBX/repo/one.txt"
commit "$SBX/repo" "seed" one.txt
git -C "$SBX/repo" branch feature/attach
echo 'two' >"$SBX/repo/two.txt"
commit "$SBX/repo" "next" two.txt
