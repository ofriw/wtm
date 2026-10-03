#!/usr/bin/env bash
# The child forks from base-branch, so the seeded harness must come from
# wt-base: stdout names it and the manifest pins base's marker, not main's.
wtm add wt-child refs/heads/base-branch --no-index
