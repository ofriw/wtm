#!/usr/bin/env bash
# 1) remove UNUSED wt1 (session purged); 2) the same command must now fail with
# rc=1; 3) --all must skip ACTIVE wt2 and report nothing to do.
wtm gc --path ../wt1 --yes
wtm gc --path ../wt1 --yes; echo "rc=$?"
wtm gc --all --yes
