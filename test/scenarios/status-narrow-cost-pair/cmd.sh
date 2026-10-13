#!/usr/bin/env bash
# At COLUMNS=40 the table keeps the proven pair whole ($0.00/$41.20) while the
# identity columns drop with stderr disclosure — never middle-elided into a
# wrong number, never fallen back to billed-only. Run from the linked worktree
# so the detail block carries the priced rows. (The width-20 drop end lives in
# unit tests: the summary block still middle-elides there, which needs its own
# fix before a golden may pin it.)
. "${HARNESS:-/harness}/lib.sh"
cd "$SBX/wt"
COLUMNS=40 wtm status
