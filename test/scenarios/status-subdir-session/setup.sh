#!/usr/bin/env bash
# The only session runs from a repo subdirectory, so no session cwd equals the
# worktree. Its ACTIVE and COST must still count for the containing worktree.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt
mkdir -p "$SBX/wt/website"

mk_pi_usage_session "$SBX/wt/website" subdir "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}' \
  '{"type":"message","timestamp":"2020-01-01T01:40:00Z","message":{"role":"assistant","model":"deepseek-flash","stopReason":"stop","usage":{"cost":{"total":5.15}}}}'
# The session is newer than the git metadata, so LAST USED proves it was seen.
touch -t 202001020000 "$PI_CODING_AGENT_DIR/sessions/subdir/subdir.jsonl"

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
