#!/usr/bin/env bash
# Inject a failing chunkhound so `wtm add` fails after `git worktree add`.
mkdir -p /tmp/failbin
printf '#!/bin/sh\nexit 1\n' >/tmp/failbin/chunkhound
chmod +x /tmp/failbin/chunkhound
export PATH="/tmp/failbin:$PATH"

# Case 1: the checkout is pristine, so the partial worktree and branch go away.
if wtm add wt-clean >/dev/null 2>&1; then
  echo 'add unexpectedly succeeded on a clean seed' >&2
  exit 1
fi
[[ ! -d ../repo-wt-clean ]] || { echo 'clean partial worktree kept' >&2; exit 1; }
if git rev-parse -q --verify refs/heads/wt-clean >/dev/null; then
  echo 'clean partial branch kept' >&2
  exit 1
fi

# Case 2: an attached pre-existing branch must survive a failed seed even when
# the pristine checkout is rolled back.
git branch keep-branch
wtm add keep-branch >/dev/null 2>&1 && { echo 'add unexpectedly succeeded when attaching' >&2; exit 1; }
git rev-parse -q --verify refs/heads/keep-branch >/dev/null || { echo 'attached branch was deleted' >&2; exit 1; }
[[ ! -d ../repo-keep-branch ]] || { echo 'clean attached partial worktree kept' >&2; exit 1; }

# Case 3: the seed copies an untracked .mcp.json, so the partial worktree must
# survive for inspection and its branch must remain.
echo '{"mcpServers":{}}' >.mcp.json
if err=$(wtm add wt-keep 2>&1 >/dev/null); then
  echo 'add unexpectedly succeeded on a seeded worktree' >&2
  exit 1
fi
grep -q 'partial worktree kept' <<<"$err" || { echo "recovery hint missing: $err" >&2; exit 1; }
[[ -f ../repo-wt-keep/.mcp.json ]] || { echo 'kept worktree lost its seeded file' >&2; exit 1; }
if ! git rev-parse -q --verify refs/heads/wt-keep >/dev/null; then
  echo 'kept partial branch was deleted' >&2
  exit 1
fi
echo 'add failure safety passed'
echo 'attach failure safety passed'
