#!/usr/bin/env bash
# Default TTL, then the persisted value, both as machine output.
wtm config --json
wtm config set unusedTTL 30d >/dev/null
wtm config --json
