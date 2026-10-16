#!/usr/bin/env bash
# Seed: a plain repo with a committed file. The arg-count check must reject
# before any worktree is looked up, so the seed never decides the outcome.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
