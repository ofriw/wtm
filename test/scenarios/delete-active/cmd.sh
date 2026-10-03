#!/usr/bin/env bash
# ACTIVE yet named by branch: delete removes the checkout, purges sessions and
# force-deletes the local branch the command is addressed by.
wtm delete feature/dead --yes
if git rev-parse -q --verify refs/heads/feature/dead >/dev/null 2>&1; then
  echo 'local branch survived' >&2
  exit 1
fi
