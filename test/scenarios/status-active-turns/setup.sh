#!/usr/bin/env bash
# Two agent turns with a two-hour idle gap between them. ACTIVE must count the
# turns (tool run included) and exclude the gap; the old first-to-last span
# would report the whole three hours.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_pi_usage_session "$SBX/wt" pi-turns "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"start"}]}}' \
  '{"type":"message","timestamp":"2020-01-01T00:01:00Z","message":{"role":"assistant","model":"deepseek-flash","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"bash"}],"usage":{"cost":{"total":1.00}}}}' \
  '{"type":"message","timestamp":"2020-01-01T00:45:00Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"done"}]}}' \
  '{"type":"message","timestamp":"2020-01-01T00:46:00Z","message":{"role":"assistant","model":"deepseek-flash","stopReason":"stop","usage":{"cost":{"total":0.50}}}}' \
  '{"type":"message","timestamp":"2020-01-01T03:00:00Z","message":{"role":"user","content":[{"type":"text","text":"again"}]}}' \
  '{"type":"message","timestamp":"2020-01-01T03:05:00Z","message":{"role":"assistant","model":"deepseek-flash","stopReason":"stop","usage":{"cost":{"total":0.25}}}}'

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
