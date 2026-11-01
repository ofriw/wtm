#!/usr/bin/env bash
# Seed: main plus an existing mixed-case Feature/Attach branch one commit ahead,
# with a configured upstream. The lowercase request must adopt it, never create
# a second branch.
. "${HARNESS:-/harness}/lib.sh"

seed_repo

git -C "$SBX/repo" checkout -q -b Feature/Attach
echo 'attached' >"$SBX/repo/attach.txt"
commit "$SBX/repo" "attach" attach.txt
git -C "$SBX/repo" checkout -q main
git -C "$SBX/repo" config branch.Feature/Attach.remote origin
git -C "$SBX/repo" config branch.Feature/Attach.merge refs/heads/Feature/Attach
printf '{"from":"main"}\n' >"$SBX/repo/.chunkhound.json"
