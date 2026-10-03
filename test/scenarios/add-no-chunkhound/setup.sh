#!/usr/bin/env bash
# Seed: clean repo (main) with a commit and no ChunkHound workspace. `wtm add`
# must succeed even when chunkhound is absent; cmd.sh hides it from PATH.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
