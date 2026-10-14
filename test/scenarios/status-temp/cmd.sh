#!/usr/bin/env bash
# COLUMNS pins the fit so the TEMP column is never shrunk away. Main shows "-"
# and the temp row shows "expired"; no remaining window reaches the golden.
COLUMNS=120 wtm status
