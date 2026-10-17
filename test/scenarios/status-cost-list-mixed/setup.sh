#!/usr/bin/env bash
# One metered Pi worktree beside one billing through an OAuth-only provider
# (openai-codex), so the session record alone proves the subscription and only
# that row carries a list price. The table must therefore render a COST column
# of single prices with a LIST column of dashes beside $20.60, each header over
# its own numbers; the breakdown pins the same split for the current worktree.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt
worktree_add "$SBX/repo" "$SBX/wt-sub" wt-sub

mk_pi_usage_session "$SBX/wt" pi-usage "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"assistant","model":"deepseek-flash","usage":{"cost":{"total":2.90}}}}' \
  '{"type":"message","timestamp":"2020-01-01T01:00:00Z","message":{"role":"assistant","model":"deepseek-flash","usage":{"cost":{"total":1.10}}}}'

mk_pi_usage_session "$SBX/wt-sub" pi-sub "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"assistant","provider":"openai-codex","model":"gpt-5","usage":{"cost":{"total":20.60}}}}' \
  '{"type":"message","timestamp":"2020-01-01T00:45:00Z","message":{"role":"assistant","provider":"openai-codex","model":"gpt-5","usage":{"cost":{"total":20.60}}}}'

wtm config set unusedTTL 36500d
# .git mtime is the AGE baseline and the session files are activity; backdate
# the worktree metadata so the fixed 2020 transcripts decide LAST USED, while
# the session events decide ACTIVE and COST.
backdate "$SBX/repo/.git" "$SBX/wt/.git" "$SBX/wt-sub/.git"
