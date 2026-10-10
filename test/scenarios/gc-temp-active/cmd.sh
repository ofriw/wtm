#!/usr/bin/env bash
# The temp is inside its window, so --all selects nothing and the store record
# must survive the reconcile pass.
out=$(wtm gc --all --yes)
printf '%s\n' "$out"
[[ -s "$HOME/.wtm/temp.json" ]] || { echo 'temp store emptied' >&2; exit 1; }
grep -qF "$SBX/repo-tmp-x" "$HOME/.wtm/temp.json" || { echo 'temp record vanished' >&2; exit 1; }
