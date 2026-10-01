#!/usr/bin/env bash
# wt1 must be removed but its session file must survive byte-for-byte.
wtm gc --all --yes --keep-sessions
