#!/usr/bin/env bash
# Seed: a worktree whose path alone is wider than an 80-column terminal, so the
# PATH column must middle-elide while every other column survives intact. The
# fixed 2020 commit keeps LAST USED deterministic; the repo's own row is short.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

long="$SBX/a/very/long/nested/checkout/path/that/keeps/going/and/going/repo-checkout"
worktree_add "$SBX/repo" "$long" long-path-branch

backdate "$SBX/repo/.git" "$long/.git"
