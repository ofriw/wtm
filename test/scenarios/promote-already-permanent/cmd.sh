#!/usr/bin/env bash
# Promoting a permanent worktree is the desired end state, so it must be an
# explicit success (exit 0), not an error: retries are safe.
wtm promote wt-perm
