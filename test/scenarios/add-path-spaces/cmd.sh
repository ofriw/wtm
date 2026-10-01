#!/usr/bin/env bash
# --no-index keeps the offline stub out of this path-shape scenario. An explicit
# valid -b is required: the default branch name (path base) would contain spaces.
wtm add "../wt with space" -b wt-spaced --no-index
if ! wtm status --json | grep -qF '"path": "/sbx/wt with space"'; then
  echo 'spaced worktree missing from status' >&2
  exit 1
fi
echo 'spaced path checks passed'
