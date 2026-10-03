#!/usr/bin/env bash
# The new branch must fork from main; both log lines prove it by subject. The
# checkout directory is derived as <repo>-<branch-slug> next to the repo.
wtm add wtbase
echo "repo-wtbase: $(git -C ../repo-wtbase log -1 --format='%h %s')"
echo "main:    $(git log -1 --format='%h %s')"
