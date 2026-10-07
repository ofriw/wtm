#!/usr/bin/env bash
# Seed: a plain repo with a committed file. The branch lookup must fail before
# anything is touched.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
