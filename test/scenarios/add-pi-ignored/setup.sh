#!/usr/bin/env bash
# Seed: .pi/local.json is git-ignored and untracked; .pi/tracked.json is
# committed; .pi/loud.json is untracked and NOT ignored. Only the ignored file
# may be carried over by wtm.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
printf '.pi/local.json\n' >"$SBX/repo/.gitignore"
mkdir -p "$SBX/repo/.pi"
echo '{"tracked":true}' >"$SBX/repo/.pi/tracked.json"
commit "$SBX/repo" "seed" .gitignore .pi/tracked.json

echo '{"secret":true}' >"$SBX/repo/.pi/local.json"
echo '{"loud":true}' >"$SBX/repo/.pi/loud.json"
