#!/usr/bin/env bash
# One Pi session in a linked worktree. Running wtm from inside that worktree must
# mark it `current` and print its usage detail block, not the main worktree's.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_pi_usage_session "$SBX/wt" pi-usage "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"assistant","model":"deepseek-flash","usage":{"cost":{"total":2.90}}}}' \
  '{"type":"message","timestamp":"2020-01-01T00:45:00Z","message":{"role":"assistant","model":"deepseek-flash","usage":{"cost":{"total":1.10}}}}'

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
