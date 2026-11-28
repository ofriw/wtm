#!/usr/bin/env bash
# At COLUMNS=60 the capability columns are elided whole (shrinkRank 1, floor
# 0), then AGE/ACTIVE/COST give up width, leaving the identity columns and STATUS.
# The grouped INTEGRATIONS cell in the default view covers the capability state.
COLUMNS=60 wtm status --wide
