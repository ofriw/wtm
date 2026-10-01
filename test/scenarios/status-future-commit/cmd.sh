#!/usr/bin/env bash
# Assert on the STATUS column only: the clamped LAST USED date is today,
# which must never enter a golden.
. "${HARNESS:-/harness}/lib.sh"
out=$(wtm status)
status_of() { awk -v p="$1" '$1==p {print $4}' <<<"$out"; }
[[ $(status_of "$SBX/wt-future") == ACTIVE ]] || { echo 'wt-future not ACTIVE' >&2; exit 1; }
echo 'future-commit checks passed'
