#!/usr/bin/env bash
# --no-index keeps the comparison purely about the Pi harness files: the
# source's .mcp.json must arrive in the new checkout (golden manifest pins
# it) and stdout must name the source worktree.
wtm add ../wt-from --from ../wt-src --no-index
