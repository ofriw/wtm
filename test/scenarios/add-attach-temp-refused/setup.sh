#!/usr/bin/env bash
# Seed: tmp/keep already exists. --temp must refuse to adopt it so gc never
# inherits a branch wtm did not create.
. "${HARNESS:-/harness}/lib.sh"

seed_repo
git -C "$SBX/repo" branch tmp/keep
