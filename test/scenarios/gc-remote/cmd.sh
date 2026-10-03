#!/usr/bin/env bash
set -euo pipefail
# Default gc removes the worktree and deletes its upstream branch on origin.
# The worktree's local branch is not the target and must survive: gc reclaims
# checkouts, it never deletes a branch that later work may still need.
wtm gc --path ../wt1 --yes
git rev-parse --verify refs/heads/wt1 >/dev/null
