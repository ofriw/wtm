#!/usr/bin/env bash
# Seed: two local branches differ only by case, so a request that matches neither
# spelling exactly is ambiguous.
. "${HARNESS:-/harness}/lib.sh"

seed_repo
git -C "$SBX/repo" update-ref refs/heads/Feature/X HEAD
git -C "$SBX/repo" update-ref refs/heads/feature/x HEAD
