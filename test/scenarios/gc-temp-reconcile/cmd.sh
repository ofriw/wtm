#!/usr/bin/env bash
# The orphaned record vanishes in reconcile (before selection), so no checkout
# is removed and the registry reads empty. The registry assertion mirrors
# gc-temp/cmd.sh; the stdout golden pins `nothing to do`.
wtm gc --all --yes
[[ $(tr -d '[:space:]' <"$HOME/.wtm/temp.json") == '{}' ]] || { echo 'orphaned temp record survived gc' >&2; exit 1; }
