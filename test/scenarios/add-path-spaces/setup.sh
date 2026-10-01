#!/usr/bin/env bash
# Seed: plain repo. A target path with spaces exercises the porcelain parser,
# which separates records on blank lines rather than on spaces.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
