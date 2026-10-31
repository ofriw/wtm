#!/usr/bin/env bash
# Seed: a linked worktree already holds feature/busy. Attachment must fail and
# name that worktree instead of creating a second checkout.
. "${HARNESS:-/harness}/lib.sh"

seed_repo
worktree_add "$SBX/repo" "$SBX/wt-busy" feature/busy main
