#!/usr/bin/env bash
# COLUMNS is the only input the fit depends on; pinning it makes the golden
# independent of the absent TTY.
COLUMNS=80 wtm status
