#!/usr/bin/env bash
# Default delete removes the worktree, deletes the remote upstream and drops the
# local branch.
wtm delete feature/remote --yes
if git rev-parse -q --verify refs/heads/feature/remote >/dev/null 2>&1; then
  echo 'local branch survived' >&2
  exit 1
fi
