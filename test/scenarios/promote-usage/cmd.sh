#!/usr/bin/env bash
# promote takes exactly one branch: 0 args and 2+ args are both usage errors
# (rc=2, nothing on stdout). The first case is pinned by echoing its rc (as
# gc-remove does); the second is the script's own exit, pinned by expected.rc.
wtm promote; echo "no-args rc=$?"
wtm promote a b
