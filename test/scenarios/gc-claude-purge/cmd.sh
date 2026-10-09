#!/usr/bin/env bash
set -euo pipefail
wtm gc --path ../wt1 --yes
[[ ! -e "$CLAUDE_CONFIG_DIR/projects/-sbx-wt1" ]]
