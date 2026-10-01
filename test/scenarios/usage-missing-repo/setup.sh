#!/usr/bin/env bash
# Seed: $SBX/repo exists but is NOT a git repo. entry.sh cd's into it, so a bare
# `wtm status` cannot resolve a root and must exit 1.
. "${HARNESS:-/harness}/lib.sh"

mkdir -p "$SBX/repo"
