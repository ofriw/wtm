#!/usr/bin/env bash
# Table first, machine output second: both must report the configured remote
# upstream (a real non-"-" UPSTREAM cell), which the other status goldens lack.
wtm status
wtm --json status
