#!/usr/bin/env bash
# Seed: .pi/mcp.json is untracked and NOT git-ignored. The generalized MCP seed
# must still carry it into the checkout, unlike other .pi/ files.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

mkdir -p "$SBX/repo/.pi"
printf '{"mcpServers":{"chunkhound":{"command":"chunkhound","args":["mcp"]}}}\n' >"$SBX/repo/.pi/mcp.json"
