#!/usr/bin/env bash
# At COLUMNS=20 the table cannot keep COST: it drops whole with disclosure,
# never middle-elided into a wrong number. The detail block still keeps TIME
# and COST. Run from the linked worktree so the block carries priced rows.
. "${HARNESS:-/harness}/lib.sh"
cd "$SBX/wt"
COLUMNS=20 wtm status
