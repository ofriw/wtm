#!/usr/bin/env bash
# A rejected remote delete must fail gc with a clear error.
wtm gc --path ../wt1 --yes
