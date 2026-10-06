#!/usr/bin/env bash
# The worktree and branch go away; the Claude session file must survive.
set -euo pipefail
wtm delete feature/dead --yes --keep-sessions
