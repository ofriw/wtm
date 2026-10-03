#!/usr/bin/env bash
# ChunkHound is optional: hide it from PATH (the harness puts the offline stub
# on /stub-bin). Prefixed branches keep the convention warning out of case 1,
# which asserts silence. Case 2 gives the project an active workspace, so the
# skipped index must warn — the copied db is no longer refreshed.
. "${HARNESS:-/harness}/lib.sh"
export PATH="/usr/local/bin:/usr/bin:/bin"

if ! wtm add feature/wt-plain 2>/tmp/wt-plain.err; then
  echo 'add failed without chunkhound' >&2
  exit 1
fi
[[ ! -s /tmp/wt-plain.err ]] || { echo "unexpected stderr: $(cat /tmp/wt-plain.err)" >&2; exit 1; }

# An active workspace: config + db, created by the offline stub by absolute path
# since PATH no longer resolves it. Its output is setup noise, not the contract.
printf '{"llm":{"provider":"openai","model":"gpt-4o-mini"}}\n' >"$SBX/repo/.chunkhound.json"
/stub-bin/chunkhound index "$SBX/repo" >/dev/null 2>&1
if ! wtm add feature/wt-stale; then
  echo 'add failed for a workspace without chunkhound' >&2
  exit 1
fi
