#!/usr/bin/env bash
# Seed: plain repo. config --json must report the resolved settings path and TTL.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
