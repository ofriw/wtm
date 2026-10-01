#!/usr/bin/env bash
# Seed: main repo plus a linked source worktree carrying an untracked
# .mcp.json. `wtm add --from` must seed the new checkout from the source, not
# from main.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt-src" wt-src
printf '{"mcpServers":{"chunkhound":{"command":"chunkhound","args":["mcp"]}}}\n' >"$SBX/wt-src/.mcp.json"
