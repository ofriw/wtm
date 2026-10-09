#!/usr/bin/env bash
set -euo pipefail
wtm delete feature/dead --yes
[[ ! -e "$CLAUDE_CONFIG_DIR/projects/-sbx-wt1" ]]
