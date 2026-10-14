#!/usr/bin/env bash
# The machine report must carry the same facts as the human one, and nil slices
# must serialize as [] — a null would break consumers that iterate blindly.
out=$(wtm gc --all --yes --json)
printf '%s\n' "$out"
if grep -q 'null' <<<"$out"; then
  echo 'gc --json emitted null' >&2
  exit 1
fi
[[ $(tr -d '[:space:]' <"$HOME/.wtm/temp.json") == '{}' ]] || { echo 'temp store not emptied' >&2; exit 1; }
