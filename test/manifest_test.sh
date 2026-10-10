#!/usr/bin/env bash
# Exercise real manifest validation; command substitution must not hide failure.
set -euo pipefail
harness=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/.git/worktrees/temp"
token="$root/.git/worktrees/temp/wtm-temp-identity"
printf '%064d' 1 >"$token"
first=$(bash "$harness/manifest.sh" "$root")
printf '%064d' 2 >"$token"
second=$(bash "$harness/manifest.sh" "$root")
[[ $first == "$second" ]] || { echo 'valid identities must normalize equally' >&2; exit 1; }
reject_identity() {
  if bash "$harness/manifest.sh" "$root" >/dev/null; then
    echo 'manifest accepted an invalid identity' >&2
    exit 1
  fi
}
printf 'invalid' >"$token"
reject_identity
printf '%064d\nextra' 1 >"$token"
reject_identity
