#!/usr/bin/env bash
# Emit a normalized manifest of a sandbox tree, one line per entry:
#   <type>\t<mode>\t<sha256>\t<path>
# Sorted LC_ALL=C. Paths are re-rooted at /SBX so goldens are sandbox-location
# independent. Hashes of known nondeterministic files become "volatile" while
# type+mode stay asserted, so existence is still pinned.
set -euo pipefail

root=${1:-/sbx}
prefix=${2:-/SBX}

# Evidence-based volatility: DuckDB bytes and git indexes/reflogs embed
# wall-clock or inode state even when all inputs are fixed.
volatile_path() {
  case "$1" in
    */chunks.db | */chunks.db.wal) return 0 ;;
    */.git/index) return 0 ;;
    */.git/worktrees/*/index) return 0 ;;
    */.git/logs/*) return 0 ;;
    */.git/worktrees/*/logs/*) return 0 ;;
  esac
  return 1
}

# Keep registry keys, branch, TTL, and all schema fields in the digest.
# Only random identities, creation times, and sandbox roots vary between runs.
normalized_temp_store() {
  python3 - "$1" "$root" "$prefix" <<'PY'
import datetime, json, re, sys
path, root, prefix = sys.argv[1:]
with open(path) as source:
    store = json.load(source)
assert isinstance(store, dict), 'temp registry must be an object'
for record in store.values():
    assert re.fullmatch('[0-9a-f]{64}', record['identity']), 'invalid temp identity'
    datetime.datetime.fromisoformat(record['createdAt'].replace('Z', '+00:00'))
    record['identity'] = '<identity>'
    record['createdAt'] = '<createdAt>'
store = {key.replace(root + '/', prefix + '/', 1): value for key, value in store.items()}
print(json.dumps(store, sort_keys=True, separators=(',', ':')))
PY
}

normalized_hash() {
  case "$1" in
    */.wtm/temp.json) normalized_temp_store "$1" | sha256sum | cut -d' ' -f1 ;;
    */.git/worktrees/*/wtm-temp-identity)
      [[ $(wc -c <"$1") -eq 64 ]] || return 1
      grep -qxE '[0-9a-f]{64}' "$1" || return 1
      printf '<identity>\n' | sha256sum | cut -d' ' -f1 ;;
    *) sha256sum "$1" | cut -d' ' -f1 ;;
  esac
}

emit() {
  local p=$1 rel type mode sha
  rel=${p#"$root"}
  if [[ -L $p ]]; then
    type=l
    mode=$(stat -c %a "$p")
    sha=$(readlink "$p" | sha256sum | cut -d' ' -f1)
  elif [[ -d $p ]]; then
    type=d
    mode=$(stat -c %a "$p")
    sha=-
  else
    type=f
    mode=$(stat -c %a "$p")
    if volatile_path "$rel"; then
      sha=volatile
    else
      sha=$(normalized_hash "$p")
    fi
  fi
  printf '%s\t%s\t%s\t%s\n' "$type" "$mode" "$sha" "$prefix$rel"
}

find "$root" -mindepth 1 -print0 | LC_ALL=C sort -z | while IFS= read -r -d '' p; do
  emit "$p"
done
