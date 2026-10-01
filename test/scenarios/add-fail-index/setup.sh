#!/usr/bin/env bash
# Seed: clean repo with a single commit. A failing chunkhound index must clean
# up an unmodified partial worktree, but keep one that already holds seeded
# files. cmd.sh injects the failing indexer via PATH; setup keeps the db_index
# contract (offline stub) intact.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
