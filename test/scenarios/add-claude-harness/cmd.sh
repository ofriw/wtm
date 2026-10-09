#!/usr/bin/env bash
set -euo pipefail
wtm add wt-claude
[[ -f ../repo-wt-claude/.claude/settings.local.json ]]
[[ -f ../repo-wt-claude/CLAUDE.local.md ]]
