#!/usr/bin/env bash
# COLUMNS=100 is six cells short of the detail grid's natural width: LIST is
# absent and MODEL must elide so the token group survives.
. "${HARNESS:-/harness}/lib.sh"
cd "$SBX/wt"
COLUMNS=100 wtm status
