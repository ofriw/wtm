#!/usr/bin/env bash
# stderr is a pipe in the harness, so auto progress must disable: operations that
# request progress (like gc) must emit no ANSI escapes at all on a non-TTY stream.
tmp=$(mktemp)
wtm gc --all --yes >/dev/null 2>"$tmp"
if grep -q $'\x1b' "$tmp"; then
  echo "progress-non-tty: gc stderr contains escape bytes" >&2
  cat "$tmp" >&2
  rm -f "$tmp"
  exit 1
fi
wtm status 2>"$tmp"
rc=$?
if grep -q $'\x1b' "$tmp"; then
  echo "progress-non-tty: stderr contains escape bytes" >&2
  cat "$tmp" >&2
  rm -f "$tmp"
  exit 1
fi
cat "$tmp" >&2
rm -f "$tmp"
exit "$rc"
