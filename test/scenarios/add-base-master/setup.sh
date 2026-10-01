#!/usr/bin/env bash
# Seed: a repo whose only branch is master (git init -b master). The main probe
# must fall back from main to master.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" master
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
