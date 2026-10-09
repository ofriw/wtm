#!/usr/bin/env bash
# Table first, wide view second, machine output third, GC last: the golden pins
# ACTIVE+claude, the CLAUDE=yes wide column (plus n/a vs - per agent), the
# claude:"yes" JSON contract, and the preserving GC pass.
wtm status
wtm status --wide
wtm --json status
wtm gc --all --yes
