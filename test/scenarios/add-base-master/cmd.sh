#!/usr/bin/env bash
# Base must resolve to refs/heads/master, and the new commit must match master.
wtm add ../wt-master
echo "wt-master: $(git -C ../wt-master log -1 --format='%h %s')"
echo "master:    $(git log -1 --format='%h %s')"
