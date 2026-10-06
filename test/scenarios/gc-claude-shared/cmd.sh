#!/usr/bin/env bash
# wt1's transcript is purged; the peer transcript sharing its project dir — and
# the dir itself — must survive. The golden manifest pins both survivors.
set -euo pipefail
wtm gc --path ../wt1 --yes
