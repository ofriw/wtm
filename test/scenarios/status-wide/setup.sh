#!/usr/bin/env bash
# Seed: --wide expands every registered capability into its own column. Main is
# fully wired (chunkhound+mcp); the linked worktree has only .chunkhound.json,
# so chunkhound reads partial and mcp reads the applicable-but-absent "-".
# claude is registered but used nowhere, so it reads n/a — the distinction from
# an applicable-but-absent "-". A very long TTL keeps the fixed 2020 sessions
# ACTIVE so LAST USED stays deterministic.
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
# .git is the creation baseline and untracked configs are activity; backdate all
# so the fixed session date decides LAST USED rather than setup wall-clock.
backdate "$SBX/repo/.git" "$SBX/wt-partial/.git" "$SBX/repo/.mcp.json" "$SBX/wt-partial/.chunkhound.json"
