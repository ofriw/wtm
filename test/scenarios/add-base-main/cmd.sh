#!/usr/bin/env bash
# The new branch must fork from main; both log lines prove it by subject.
wtm add ../wt-base -b wtbase
echo "wt-base: $(git -C ../wt-base log -1 --format='%h %s')"
echo "main:    $(git log -1 --format='%h %s')"
