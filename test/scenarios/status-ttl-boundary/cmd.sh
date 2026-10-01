#!/usr/bin/env bash
# The cutoff is exclusive: age == ttl is ACTIVE, age > ttl is UNUSED. Assert on
# the STATUS column so the wall-clock LAST USED date never enters a golden.
. "${HARNESS:-/harness}/lib.sh"
out=$(wtm status)
status_of() { awk -v p="$1" '$1==p {print $4}' <<<"$out"; }
[[ $(status_of "$SBX/wt-fresh") == ACTIVE ]] || { echo 'wt-fresh not ACTIVE' >&2; exit 1; }
[[ $(status_of "$SBX/wt-edge") == ACTIVE ]] || { echo 'wt-edge not ACTIVE' >&2; exit 1; }
[[ $(status_of "$SBX/wt-old") == UNUSED ]] || { echo 'wt-old not UNUSED' >&2; exit 1; }

# GC shares the same TTL: only the beyond-cutoff worktree may be collected.
gc_out=$(wtm gc --all --yes)
grep -qx "removed $SBX/wt-old" <<<"$gc_out" || { echo 'wt-old not collected' >&2; exit 1; }
if grep -q 'wt-edge\|wt-fresh' <<<"$gc_out"; then
  echo 'an ACTIVE worktree was collected' >&2
  exit 1
fi
echo 'TTL boundary checks passed'
