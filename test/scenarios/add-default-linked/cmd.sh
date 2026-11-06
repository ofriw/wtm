#!/usr/bin/env bash
set -euo pipefail
# The branch forks origin/main, but the harness must come from the checked out
# main worktree (stdout + marker pin it); no db here, so no index may be claimed.
wtm add wt-child --no-index 2>/tmp/add.err
cat /tmp/add.err >&2
grep -q "ChunkHound index" /tmp/add.err && { echo 'warning claims an absent ChunkHound index' >&2; exit 1; }
echo "marker: $(cat ../repo-wt-child/.chunkhound.json)"
