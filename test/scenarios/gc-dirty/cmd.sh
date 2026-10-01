#!/usr/bin/env bash
# Explicit --path selection is consent: the dirty worktree is removed outright,
# with no --force flag and no second prompt. The gc picker surfaces dirty state
# to make the interactive choice informed.
wtm gc --path ../dirty --yes </dev/null
