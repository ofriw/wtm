#!/usr/bin/env bash
# A start-point that disagrees with the branch tip is a usage error; nothing is
# attached and the branch is not moved.
wtm add feature/attach main --no-index
