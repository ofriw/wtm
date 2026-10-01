#!/usr/bin/env bash
# No `set -e` here (mirrors gc-remove): the failure is the assertion. The
# nonzero exit propagates as cmd.sh's own so entry.sh pins it via
# expected.rc, and the error text via expected.stderr-substr.
wtm gc --path ../nope --yes
