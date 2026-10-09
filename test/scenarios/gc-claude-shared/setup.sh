#!/usr/bin/env bash
# Seed: wt1 UNUSED and wt2 both own one transcript in a single shared Claude
# project dir. Ownership comes from the cwd header, never the directory name.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt1" wt1
worktree_add "$SBX/repo" "$SBX/wt2" wt2
mk_claude_session "$SBX/wt1" "sess-wt1" "$OLD_STAMP"
mk_claude_session "$SBX/wt2" "sess-wt2" "$FUTURE_STAMP"

# Collapse both transcripts into one project dir so wt1's purge must spare its
# neighbour: the shared dir stays non-empty, so no cleanup may remove it.
shared="$CLAUDE_CONFIG_DIR/projects/shared"
mkdir -p "$shared"
mv "$CLAUDE_CONFIG_DIR/projects/-sbx-wt1/sess-wt1.jsonl" "$shared/"
mv "$CLAUDE_CONFIG_DIR/projects/-sbx-wt2/sess-wt2.jsonl" "$shared/"
rmdir "$CLAUDE_CONFIG_DIR/projects/-sbx-wt1" "$CLAUDE_CONFIG_DIR/projects/-sbx-wt2"
backdate "$SBX/wt1/.git"
