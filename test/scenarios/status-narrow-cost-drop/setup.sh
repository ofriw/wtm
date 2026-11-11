#!/usr/bin/env bash
# Same seed as status-cost-pi (one Pi session with priced events); the command
# runs at 20 columns from inside the priced worktree, so the golden pins the
# last-resort policy end to end: COST drops whole (never cut) with stderr
# disclosure, while the detail block keeps TIME/COST and drops identity first.
. "${HARNESS:-/harness}/scenarios/status-cost-pi/setup.sh"
