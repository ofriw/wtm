#!/usr/bin/env bash
# Seed: permanent main plus an ORPHANED temp record whose checkout never
# existed on disk. Reconcile drops it before selection, so gc reports nothing
# to do. Fields mirror a real add with fixed createdAt, keeping the golden
# wall-clock free; the manifest normalizer pins identity and createdAt shape.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

# A very long TTL keeps the backdated main ACTIVE, so no checkout qualifies.
wtm config set unusedTTL 36500d
backdate "$SBX/repo" "$SBX/repo/.git"

# Hand-written like set_temp_window in temp-lib.sh: a canonical but absent
# checkout path, which reconcile proves missing and drops.
mkdir -p "$HOME/.wtm"
python3 - "$HOME/.wtm/temp.json" "$SBX/repo-tmp-gone" <<'PY'
import json, sys
registry, checkout = sys.argv[1:]
store = {checkout: {
    'createdAt': '2020-01-01T00:00:00Z',
    'ttl': '1h',
    'branch': 'tmp/gone',
    'identity': 'ab' * 32,
}}
with open(registry, 'w') as target:
    json.dump(store, target, indent=2)
    target.write('\n')
PY
