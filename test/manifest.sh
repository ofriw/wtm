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
      sha=$(sha256sum "$p" | cut -d' ' -f1)
    fi
  fi
  printf '%s\t%s\t%s\t%s\n' "$type" "$mode" "$sha" "$prefix$rel"
}

find "$root" -mindepth 1 -print0 | LC_ALL=C sort -z | while IFS= read -r -d '' p; do
  emit "$p"
done
