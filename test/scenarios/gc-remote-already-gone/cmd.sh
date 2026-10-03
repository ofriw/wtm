#!/usr/bin/env bash
# The upstream branch is already gone: gc removes the worktree and purges its
# session, reports no remote delete, and still exits 0.
wtm gc --path ../wt1 --yes
