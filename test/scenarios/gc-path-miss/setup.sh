#!/usr/bin/env bash
# Seed: plain repo (main). Collecting a path outside the repository's
# worktree list must fail, not silently prune.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
