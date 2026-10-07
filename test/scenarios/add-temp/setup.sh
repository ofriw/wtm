#!/usr/bin/env bash
# Seed: plain repo. `wtm add --temp` must create a tmp/ branch and record the
# checkout in ~/.wtm/temp.json. The manifest pins branch, TTL, and schema.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
