# Exercise persisted settings and validation through the public CLI.
wtm config | grep -q '90d'
wtm config set unusedTTL 30d >/dev/null
wtm config | grep -q '30d'
for bad in 0d -1d 1h 1.5d; do
  wtm config set unusedTTL "$bad" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "accepted invalid TTL: $bad" >&2
    exit 1
  fi
  # Fix B: an invalid TTL is usage, not a generic error (exit 2).
  if [ "$rc" -ne 2 ]; then
    echo "invalid TTL $bad exited rc=$rc, want 2" >&2
    exit 1
  fi
done
wtm config set unusedTTL 1d >/dev/null
wtm config | grep -q '1d'
echo 'TTL checks passed'
