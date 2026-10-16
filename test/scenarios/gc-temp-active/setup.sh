#!/usr/bin/env bash
# Seed: a temp worktree still inside its window. createdAt is now and the fresh
# checkout's .git mtime keeps LastUsed now, so gc must leave it alone.
. "${HARNESS:-/harness}/lib.sh"
. "${HARNESS:-/harness}/scenarios/temp-lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

add_temp_checkout x 1h
