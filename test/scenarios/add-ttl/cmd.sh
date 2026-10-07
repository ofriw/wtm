#!/usr/bin/env bash
# --ttl implies --temp: the branch is tmp/-prefixed and the printed window
# keeps the minutes (90m compacts to 1h30m); exactness is guaranteed in JSON.
wtm add wt-ttl --ttl 90m --no-index
