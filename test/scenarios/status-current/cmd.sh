#!/usr/bin/env bash
# Run from the linked worktree: it must be the `current` worktree, and its detail
# block must describe it, not the main worktree.
. "${HARNESS:-/harness}/lib.sh"
cd "$SBX/wt"
wtm status
wtm --json status
