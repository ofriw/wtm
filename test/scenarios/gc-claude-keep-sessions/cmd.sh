#!/usr/bin/env bash
# wt1 is removed but its Claude session file must survive byte-for-byte.
set -euo pipefail
wtm gc --path ../wt1 --yes --keep-sessions
[[ -e "$CLAUDE_CONFIG_DIR/projects/-sbx-wt1/claude-sess.jsonl" ]]
