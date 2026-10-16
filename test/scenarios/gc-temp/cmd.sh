#!/usr/bin/env bash
# An expired temp is a normal gc candidate: the checkout goes, its tmp/ branch
# is force-deleted, prune runs, and the store record is cleared. main is never
# a candidate even though its backdated mtime also reads UNUSED. The normalized
# manifest also asserts the registry is empty.
wtm gc --all --yes
[[ $(tr -d '[:space:]' <"$HOME/.wtm/temp.json") == '{}' ]] || { echo 'temp store not emptied' >&2; exit 1; }
