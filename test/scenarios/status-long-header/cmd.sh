#!/usr/bin/env bash
# Assert on the STATUS column so the wall-clock LAST USED date (the clamped
# future session lands on today) never enters a golden. ACTIVE here proves
# the oversize header was still attributed to this worktree.
. "${HARNESS:-/harness}/lib.sh"
out=$(wtm status)
status_of() { awk -v p="$1" '$1==p {print $4}' <<<"$out"; }
[[ $(status_of "$SBX/wt-big") == ACTIVE ]] || { echo 'wt-big not ACTIVE' >&2; exit 1; }
echo 'long-header checks passed'
