#!/usr/bin/env bash
# A Claude progress record larger than the 4 MiB parser cap. It carries no
# usage, so it must be skipped without tainting the readable assistant cost.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

project="$CLAUDE_CONFIG_DIR/projects/$(printf '%s' "$SBX/wt" | sed 's/[^[:alnum:]]/-/g')/claude-oversize"
mkdir -p "$project"
{
  printf '{"type":"user","cwd":"%s","message":{"role":"user","content":"seed"}}\n' "$SBX/wt"
  printf '{"type":"progress","timestamp":"2020-01-01T00:00:00Z","data":{"type":"bash_progress","pad":"'
  head -c 4200000 /dev/zero | tr '\0' a
  printf '"}}\n'
  printf '%s\n' '{"type":"assistant","timestamp":"2020-01-01T00:00:01Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","usage":{"input_tokens":100000,"output_tokens":100000,"cache_read_input_tokens":0,"cache_creation_input_tokens":10000,"cache_creation":{"ephemeral_5m_input_tokens":10000,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}'
} >"$project/claude-oversize.jsonl"
touch -t "$OLD_STAMP" "$project/claude-oversize.jsonl"

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
