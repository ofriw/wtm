#!/usr/bin/env bash
# A very long TTL keeps the fixed 2020 transcript ACTIVE so LAST USED stays
# deterministic in both table and JSON goldens. The arbitrary group name cannot
# identify its owner — only the cwd header may be used for attribution.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt1" wt1
mkdir -p "$SBX/wt1/.claude"
printf '{}\n' >"$SBX/wt1/.claude/settings.json"

mk_claude_session "$SBX/wt1" claude-sess "$OLD_STAMP"
project="$CLAUDE_CONFIG_DIR/projects/arbitrary-not-a-mangled-path"
mkdir -p "$project"
source=$(find "$CLAUDE_CONFIG_DIR/projects" -name claude-sess.jsonl)
mv "$source" "$project/claude-sess.jsonl"
rmdir "$(dirname "$source")"

wtm config set unusedTTL 36500d
# .git mtimes are the creation baselines and settings.json is activity;
# backdate all so the fixed session date decides LAST USED.
backdate "$SBX/repo/.git" "$SBX/wt1/.git" "$SBX/wt1/.claude/settings.json"
