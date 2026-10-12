# Run from the worktree so the detail block covers the mixed Pi+Claude usage:
# the table pins ACTIVE as the union (1h, not 2h) and the JSON pass pins the
# machine contract (cost sums, activeSeconds is the union).
. "${HARNESS:-/harness}/lib.sh"
cd "$SBX/wt"
wtm status
wtm --json status
