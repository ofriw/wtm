#!/usr/bin/env bash
# Container-side scenario runner: fresh sandbox → seed → snapshot → command →
# snapshot → compare against committed goldens. `--update` refreshes goldens
# and must never run unattended.
set -euo pipefail

HARNESS=${HARNESS:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}
export HARNESS
. "$HARNESS/lib.sh"

scenario=${1:?usage: entry.sh <scenario> [--update]}
update=${2:-}
dir="$HARNESS/scenarios/$scenario"
[[ -d $dir ]] || { echo "no such scenario: $scenario" >&2; exit 2; }
out="$HARNESS/tmp/out/$scenario"

export HOME="$SBX/home"
export PI_CODING_AGENT_DIR="$SBX/home/pi-agent"
export CLAUDE_CONFIG_DIR="$SBX/home/claude"
export PATH="/stub-bin:/usr/local/bin:/usr/bin:/bin"
export TZ=UTC LC_ALL=C
# Goldens opt into a width themselves (COLUMNS=80 wtm status); everything else
# must see no budget so non-TTY output stays at natural widths.
unset COLUMNS
umask 022
# Pin every git metadata field a subprocess could stamp. wtm shells out to git
# itself, so exporting in setup.sh alone would not reach `git worktree add`
# reflogs — and those bytes are compared.
export GIT_AUTHOR_NAME=wtm GIT_AUTHOR_EMAIL=wtm@example.com
export GIT_COMMITTER_NAME=wtm GIT_COMMITTER_EMAIL=wtm@example.com
export GIT_AUTHOR_DATE="$SEED_DATE" GIT_COMMITTER_DATE="$SEED_DATE"

rm -rf "$SBX" "$out"
mkdir -p "$PI_CODING_AGENT_DIR/sessions" "$CLAUDE_CONFIG_DIR/projects" "$out"

cd "$SBX"
if ! bash "$dir/setup.sh" >"$out/setup.log" 2>&1; then
  echo "--- setup failed ---" >&2
  cat "$out/setup.log" >&2
  exit 1
fi

"$HARNESS/manifest.sh" "$SBX" >"$out/before.manifest"

cd "$SBX/repo"
set +e
bash "$dir/cmd.sh" >"$out/stdout.raw" 2>"$out/stderr"
rc=$?
set -e
# stderr (chunkhound progress, error text) is deliberately not compared.
sed 's|/sbx|/SBX|g' "$out/stdout.raw" >"$out/stdout"
echo "$rc" >"$out/rc"
"$HARNESS/manifest.sh" "$SBX" >"$out/after.manifest"

# A scenario may pin a nonzero exit via expected.rc (default 0) and pin
# stderr fragments via expected.stderr-substr (one fixed string per line).
# --update refreshes stdout+manifest goldens only; it never creates these.
want_rc=0
if [[ -f $dir/expected.rc ]]; then want_rc=$(cat "$dir/expected.rc"); fi
if [[ $rc != "$want_rc" ]]; then
  echo "cmd.sh exited $rc, want $want_rc" >&2
  cat "$out/stdout" >&2
  cat "$out/stderr" >&2
  exit 1
fi

fail=0
if [[ -s $dir/expected.stderr-substr ]]; then
  while IFS= read -r pat || [[ -n $pat ]]; do
    if [[ -n $pat ]]; then
      if ! grep -qF -- "$pat" "$out/stderr"; then
        echo "stderr missing pattern: $pat" >&2
        fail=1
      fi
    fi
  done <"$dir/expected.stderr-substr"
fi
if [[ $update == --update ]]; then
  cp "$out/stdout" "$dir/expected.stdout"
  cp "$out/after.manifest" "$dir/expected.manifest"
  echo "updated $scenario goldens"
else
  diff -u "$dir/expected.stdout" "$out/stdout" || fail=1
  diff -u "$dir/expected.manifest" "$out/after.manifest" || fail=1
fi

if ((fail)); then
  echo "--- before → after (debug) ---" >&2
  diff -u "$out/before.manifest" "$out/after.manifest" >&2 || true
  echo "--- stderr ---" >&2
  cat "$out/stderr" >&2
  exit 1
fi
