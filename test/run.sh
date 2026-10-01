#!/usr/bin/env bash
# Host driver: cross-build the linux wtm binary, build the container image, then
# run scenarios inside it. Scenarios run with --network none to prove the
# offline contract (the stub injects chunkhound --no-embeddings).
#
#   test/run.sh                        verify all scenarios against goldens
#   test/run.sh --scenario status-mixed  verify one scenario
#   test/run.sh --update               regenerate goldens (inspect them!)
set -euo pipefail

TEST_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR=$(cd "$TEST_DIR/.." && pwd)
IMAGE=${WTM_TEST_IMAGE:-wtm-test:latest}
PLATFORM=${WTM_TEST_PLATFORM:-linux/$(go env GOARCH)}
UPDATE=0
BUILD=1
declare -a SCENARIOS=()

usage() {
  sed -n '2,8p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while (($# > 0)); do
  case $1 in
    --update) UPDATE=1 ;;
    --scenario) SCENARIOS+=("${2:?--scenario needs a name}"); shift ;;
    --image) IMAGE=${2:?--image needs a tag}; shift ;;
    --platform) PLATFORM=${2:?--platform needs os/arch}; shift ;;
    --no-build) BUILD=0 ;;
    -h | --help) usage; exit 0 ;;
    *) echo "run.sh: unknown option $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if ((BUILD)); then
  command -v docker >/dev/null || { echo "run.sh: docker not found" >&2; exit 1; }
  arch=${PLATFORM##*/}
  mkdir -p "$TEST_DIR/bin" "$TEST_DIR/tmp/out"
  (cd "$ROOT_DIR" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -o "$TEST_DIR/bin/wtm" .)
  # buildx+gha layer cache is CI-only (needs the docker-container driver from
  # setup-buildx-action); local runs keep plain `docker build`. --load keeps the
  # cached image visible to `docker run`.
  declare -a build=(docker build)
  if [[ -n ${WTM_CACHE_FROM:-} || -n ${WTM_CACHE_TO:-} ]]; then
    build=(docker buildx build --load)
    [[ -n ${WTM_CACHE_FROM:-} ]] && build+=(--cache-from "$WTM_CACHE_FROM")
    [[ -n ${WTM_CACHE_TO:-} ]] && build+=(--cache-to "$WTM_CACHE_TO")
  fi
  "${build[@]}" --platform "$PLATFORM" -t "$IMAGE" "$TEST_DIR"
fi

if ((${#SCENARIOS[@]} == 0)); then
  for d in "$TEST_DIR"/scenarios/*/; do
    SCENARIOS+=("$(basename "$d")")
  done
fi

declare -a PASSED=() FAILED=()
for name in "${SCENARIOS[@]}"; do
  [[ -d $TEST_DIR/scenarios/$name ]] || { echo "run.sh: no scenario $name" >&2; exit 2; }
  cmd=(bash /harness/entry.sh "$name")
  ((UPDATE)) && cmd+=(--update)
  echo "=== $name ==="
  if docker run --rm --platform "$PLATFORM" --network none \
      -v "$TEST_DIR:/harness" -e HARNESS=/harness "$IMAGE" "${cmd[@]}"; then
    PASSED+=("$name")
  else
    FAILED+=("$name")
  fi
done

echo
echo "passed: ${#PASSED[@]}  failed: ${#FAILED[@]}"
if ((${#FAILED[@]} > 0)); then
  printf 'FAILED: %s\n' "${FAILED[*]}"
  exit 1
fi
