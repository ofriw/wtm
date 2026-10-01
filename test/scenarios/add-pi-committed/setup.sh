#!/usr/bin/env bash
# Seed: .pi/settings.json is committed (arrives via checkout), .mcp.json is
# untracked (must be copied). Neither needs ignoring.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
mkdir -p "$SBX/repo/.pi"
echo '{"model":"claude"}' >"$SBX/repo/.pi/settings.json"
commit "$SBX/repo" "seed" .pi/settings.json

printf '{"mcpServers":{"chunkhound":{"command":"chunkhound","args":["mcp"]}}}\n' >"$SBX/repo/.mcp.json"
