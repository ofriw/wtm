#!/usr/bin/env bash
# Seed: permanent main plus an EXPIRED temp worktree on tmp/x. Every LAST USED
# input is pinned to SEED_DATE (commit dates via lib.sh; .git + file mtimes via
# expire_checkout; createdAt via set_temp_window), so the status table is entirely
# wall-clock free and the TEMP cell reads `expired` deterministically.
. "${HARNESS:-/harness}/lib.sh"
. "${HARNESS:-/harness}/scenarios/temp-lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

add_temp_checkout x 1h
set_temp_window "$SBX/repo-tmp-x" "2020-01-01T00:00:00Z" 1h
expire_checkout "$SBX/repo-tmp-x"
backdate "$SBX/repo" "$SBX/repo/.git"
