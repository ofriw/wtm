#!/usr/bin/env bash
# Color is a TTY feature and the harness is non-TTY, so --color forces it.
# Assert the palette is present, then prove every plain path emits no escapes.
colored=$(wtm status --color=always)
grep -q $'\x1b\[1m' <<<"$colored" || { echo 'header is not bold' >&2; exit 1; }
grep -q $'\x1b\[32m' <<<"$colored" || { echo 'no green for yes/ACTIVE' >&2; exit 1; }
grep -q $'\x1b\[33m' <<<"$colored" || { echo 'no yellow for UNUSED' >&2; exit 1; }

for mode in --color=never --color=auto; do
  if grep -q $'\x1b\[' <<<"$(wtm status "$mode")"; then
    echo "ANSI leaked with $mode" >&2
    exit 1
  fi
done
if grep -q $'\x1b\[' <<<"$(NO_COLOR=1 wtm status)"; then
  echo 'ANSI leaked with NO_COLOR' >&2
  exit 1
fi
echo 'color checks passed'
