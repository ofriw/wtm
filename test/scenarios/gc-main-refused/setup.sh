#!/usr/bin/env bash
# Seed: plain repo. Removing the main worktree would destroy the repository, so
# gc must refuse it.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
