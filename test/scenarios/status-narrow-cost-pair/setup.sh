#!/usr/bin/env bash
# One Pi session billing through an OAuth-only provider (openai-codex), so the
# session record alone proves the subscription and COST renders the
# billed/list pair ($0.00/$41.20). Otherwise the same seed shape as
# status-cost-pi: the fixed 2020 transcript decides LAST USED/ACTIVE/COST while
# WTM_CREATED_AT (or the .git birth time) sets AGE.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_pi_usage_session "$SBX/wt" pi-usage "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"assistant","provider":"openai-codex","model":"gpt-5","usage":{"cost":{"total":20.60}}}}' \
  '{"type":"message","timestamp":"2020-01-01T00:45:00Z","message":{"role":"assistant","provider":"openai-codex","model":"gpt-5","usage":{"cost":{"total":20.60}}}}'

wtm config set unusedTTL 36500d
# WTM_CREATED_AT (or the .git birth time) sets AGE. The backdated .git mtime
# sets LAST USED, while the session events decide ACTIVE and COST.
backdate "$SBX/repo/.git" "$SBX/wt/.git"
