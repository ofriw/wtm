#!/usr/bin/env bash
# Ambiguity is a usage mistake for every command, so each exits 2. No command
# may write when resolution fails.
for args in "add FEATURE/X --no-index" "delete FEATURE/X --yes" "promote FEATURE/X"; do
  rc=0
  wtm $args >/dev/null 2>&1 || rc=$?
  if [[ $rc != 2 ]]; then
    echo "wtm $args exited $rc, want 2" >&2
    exit 1
  fi
done

git rev-parse -q --verify refs/heads/Feature/X >/dev/null || exit 1
git rev-parse -q --verify refs/heads/feature/x >/dev/null || exit 1
[[ ! -d ../repo-feature-x ]] || {
  echo 'ambiguous add created a checkout' >&2
  exit 1
}
echo 'ambiguous branch exit codes passed'
