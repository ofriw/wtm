#!/usr/bin/env bash
# JSON must retain fractional TTL precision. Normalize the wall-clock deadline.
wtm add ttl-json --ttl 100ms --no-index --json | python3 -c '
import datetime, json, sys
result = json.load(sys.stdin)
datetime.datetime.fromisoformat(result["expiresAt"].replace("Z", "+00:00"))
result["expiresAt"] = "<expiresAt>"
print(json.dumps(result, indent=2))
'
