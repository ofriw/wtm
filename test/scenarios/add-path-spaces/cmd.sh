#!/usr/bin/env bash
# --no-index keeps the offline stub out of this path-shape scenario. The branch
# is a clean slug while --path carries the spaces, exercising the porcelain
# parser (records separated by blank lines, not by spaces).
wtm add wt-spaced --path "../wt with space" --no-index
if ! wtm status --json | grep -qF '"path": "/sbx/wt with space"'; then
  echo 'spaced worktree missing from status' >&2
  exit 1
fi
echo 'spaced path checks passed'
