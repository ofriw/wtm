#!/usr/bin/env bash
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
printf '.claude/settings.local.json\nCLAUDE.local.md\n' >"$SBX/repo/.gitignore"
mkdir -p "$SBX/repo/.claude"
printf '{"theme":"local"}\n' >"$SBX/repo/.claude/settings.local.json"
printf 'Local instructions.\n' >"$SBX/repo/CLAUDE.local.md"
printf 'hello\n' >"$SBX/repo/README.md"
commit "$SBX/repo" 'seed' .gitignore README.md
