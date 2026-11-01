#!/usr/bin/env bash
# A start-point that disagrees with the branch tip is an operational error
# (rc=1); nothing is attached and the branch is not moved.
wtm add feature/attach main --no-index
