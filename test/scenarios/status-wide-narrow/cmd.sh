#!/usr/bin/env bash
# At COLUMNS=60 the capability columns are elided whole (shrinkRank 1, floor
# 0) and only the core columns remain. The grouped INTEGRATIONS cell in the
# default view covers this case.
COLUMNS=60 wtm status --wide
