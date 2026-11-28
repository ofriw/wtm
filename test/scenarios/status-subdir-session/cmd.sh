#!/usr/bin/env bash
# Run from the containing worktree so the detail block is the current one.
. "${HARNESS:-/harness}/lib.sh"
cd "$SBX/wt"
COLUMNS=120 wtm status
