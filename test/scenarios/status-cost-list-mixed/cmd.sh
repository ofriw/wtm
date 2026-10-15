#!/usr/bin/env bash
# Run from the subscription worktree so the breakdown renders its LIST column,
# while the table carries one row with a list price and one without. The mixed
# table is the case that put the COST header over the list sub-field.
. "${HARNESS:-/harness}/lib.sh"
cd "$SBX/wt-sub"
wtm status
