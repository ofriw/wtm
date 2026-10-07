#!/usr/bin/env bash
# Default temp window is 1h; `wtm add` must print the temp line after the
# common fields, and the ttl must render compact (1h).
wtm add wt-temp --temp --no-index
