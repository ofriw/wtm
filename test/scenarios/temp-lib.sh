#!/usr/bin/env bash
# Live fixtures use the CLI so registry identity and Git private metadata agree.

add_temp_checkout() { # add_temp_checkout <branch> <ttl>
  (cd "$SBX/repo" && wtm add "$1" --ttl "$2" --no-index >/dev/null)
}

# Change time inputs only; never replace identity or other fields from add.
set_temp_window() { # set_temp_window <checkout> <createdAt> <ttl>
  python3 - "$HOME/.wtm/temp.json" "$1" "$2" "$3" <<'PY'
import json, sys
registry, checkout, created, ttl = sys.argv[1:]
with open(registry) as source:
    store = json.load(source)
store[checkout]['createdAt'] = created
store[checkout]['ttl'] = ttl
with open(registry, 'w') as target:
    json.dump(store, target, indent=2)
    target.write('\n')
PY
}

# Pin checkout mtimes as well as registry time to test expiry, not wall time.
expire_checkout() {
  local root=$1
  backdate "$root" "$root/.git"
  find "$root" -mindepth 1 -not -name .git -exec touch -t "$OLD_STAMP" {} +
}
