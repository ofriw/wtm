#!/usr/bin/env bash
# At COLUMNS=40 the table drops the counterfactual LIST column whole with
# stderr disclosure while the billed COST and the identity columns stay, so the
# rendered price is never middle-elided into a wrong number. Run from the
# linked worktree so the detail block carries the priced row and keeps both
# price columns side by side.
. "${HARNESS:-/harness}/lib.sh"
cd "$SBX/wt"
COLUMNS=40 wtm status
