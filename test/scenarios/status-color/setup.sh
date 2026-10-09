#!/usr/bin/env bash
# Seed: one aged worktree with a real DB and config, so the table carries an
# UNUSED status (yellow) and the --wide CHUNKHOUND cell a present integration
# (green). Color itself is asserted in cmd.sh.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

db_index "$SBX/repo"
printf '{"llm":{"provider":"openai","model":"gpt-4o-mini"}}\n' >"$SBX/repo/.chunkhound.json"
printf '{"mcpServers":{"chunkhound":{"command":"chunkhound","args":["mcp"]}}}\n' >"$SBX/repo/.mcp.json"
mk_session "$SBX/repo" "color-check" "$OLD_STAMP"
backdate "$SBX/repo/.git" "$SBX/repo/.mcp.json" "$SBX/repo/.chunkhound.json"
