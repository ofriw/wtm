#!/usr/bin/env bash
# The table pins AGE/ACTIVE/COST on an UNUSED worktree; the JSON pass pins the
# same machine contract (cost, activeSeconds, usage) independent of STATUS.
wtm status
wtm --json status
