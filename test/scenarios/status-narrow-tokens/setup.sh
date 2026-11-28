#!/usr/bin/env bash
# Long model names and a 26h30m turn make the detail grid 106 cells wide: the
# token group must survive by eliding MODEL, not by disappearing.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_pi_usage_session "$SBX/wt" narrow-tokens "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}' \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"assistant","model":"deepseek/deepseek-v4-flash-vision-exp","stopReason":"toolUse","usage":{"input":54000000,"output":20000000,"cacheRead":1900000000,"cacheWrite":500000,"cost":{"total":2.90}}}}' \
  '{"type":"message","timestamp":"2020-01-02T02:30:00Z","message":{"role":"assistant","model":"deepseek/deepseek-v4-flash-vision-exp","stopReason":"stop","usage":{"input":54000000,"output":20000000,"cacheRead":1900000000,"cacheWrite":500000,"cost":{"total":2.25}}}}' \
  '{"type":"message","timestamp":"2020-01-02T02:31:00Z","message":{"role":"assistant","model":"moonshotai/kimi-k3","stopReason":"stop","usage":{"input":3900000,"output":667100,"cacheRead":75900000,"cacheWrite":3900000,"cost":{"total":0.05}}}}'

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
