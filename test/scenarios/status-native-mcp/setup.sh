#!/usr/bin/env bash
# Seed: main worktree whose only MCP config is the built-in .pi/mcp.json. The
# pi capability must report it through the shared MCP inventory, not just
# .mcp.json, and the mcp integration must count it equally ("chunk~ mcp pi",
# mcp:"yes"). A very long TTL keeps the fixed 2020 session ACTIVE so the LAST
# USED cell is deterministic.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

db_index "$SBX/repo"

mkdir -p "$SBX/repo/.pi"
printf '{"mcpServers":{"chunkhound":{"command":"chunkhound","args":["mcp"]}}}\n' >"$SBX/repo/.pi/mcp.json"
mk_session "$SBX/repo" "zx9-native" "$OLD_STAMP"

wtm config set unusedTTL 36500d
# Backdate .git and the untracked native config so the fixed session decides
# LAST USED rather than setup wall-clock.
backdate "$SBX/repo/.git" "$SBX/repo/.pi/mcp.json"
