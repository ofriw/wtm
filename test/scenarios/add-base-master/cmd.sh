#!/usr/bin/env bash
# Base must resolve to refs/heads/master, and the new commit must match master.
wtm add wt-master
echo "repo-wt-master: $(git -C ../repo-wt-master log -1 --format='%h %s')"
echo "master:    $(git log -1 --format='%h %s')"
