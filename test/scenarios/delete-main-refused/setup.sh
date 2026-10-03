#!/usr/bin/env bash
# Seed: plain repo. Deleting the main worktree would destroy the repository.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
