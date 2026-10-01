#!/usr/bin/env bash
# Seed: a worktree whose only sign of life is a session whose 200KB first
# line dwarfs bufio.Scanner's 64 KiB limit. The header still carries this
# worktree's cwd (plus padding), so attribution must survive it: backdate the
# creation baseline and the commit so the session alone keeps the WT ACTIVE.
# `head -c`/`tr` output is fixed bytes, so the session file stays golden-safe.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

worktree_add "$SBX/repo" "$SBX/wt-big" big

d="$PI_CODING_AGENT_DIR/sessions/s-big"
mkdir -p "$d"
printf '{"id":"s-big","cwd":"%s","pad":"' "$SBX/wt-big" >"$d/s-big.jsonl"
head -c 204800 /dev/zero | tr '\0' 'x' >>"$d/s-big.jsonl"
printf '"}\n{"type":"message","text":"seed"}\n' >>"$d/s-big.jsonl"
touch -t "$FUTURE_STAMP" "$d/s-big.jsonl"

backdate "$SBX/wt-big/.git"
