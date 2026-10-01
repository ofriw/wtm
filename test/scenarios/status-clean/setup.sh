#!/usr/bin/env bash
# Seed: main worktree with a committed file, a REAL chunkhound db, an untracked
# .mcp.json, and one ACTIVE session. The session directory name is deliberately
# arbitrary — only the JSONL cwd header may be used for attribution.
#
# A very long TTL keeps the fixed 2020 session ACTIVE, so the LAST USED cell is
# deterministic. The default 90d TTL is relative to `now` and would drift with
# the clock; worktreeLastUsed also clamps future stamps to now, so a future
# session must never enter a golden.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

db_index "$SBX/repo"

printf '{"mcpServers":{"chunkhound":{"command":"chunkhound","args":["mcp"]}}}\n' >"$SBX/repo/.mcp.json"
mk_session "$SBX/repo" "zx9-not-an-encoded-name" "$OLD_STAMP"

wtm config set unusedTTL 36500d
# .git mtime is the creation baseline and the untracked .mcp.json is activity;
# backdate both so the fixed session date decides LAST USED.
backdate "$SBX/repo/.git" "$SBX/repo/.mcp.json"
