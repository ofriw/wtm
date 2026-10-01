#!/usr/bin/env bash
# Seed: plain repo. An unknown subcommand is a usage error (exit 2).
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
