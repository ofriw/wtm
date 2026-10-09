#!/usr/bin/env bash
# Seed: same as status-wide, but the command runs at COLUMNS=60 so the
# capability columns are elided and only the core columns remain.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt-partial" partial
printf '{"llm":{"provider":"openai","model":"gpt-4o-mini"}}\n' >"$SBX/wt-partial/.chunkhound.json"

db_index "$SBX/repo"
printf '{"mcpServers":{"chunkhound":{"command":"chunkhound","args":["mcp"]}}}\n' >"$SBX/repo/.mcp.json"
mk_session "$SBX/repo" "zx9-wide" "$OLD_STAMP"

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt-partial/.git" "$SBX/repo/.mcp.json" "$SBX/wt-partial/.chunkhound.json"
