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

# Scenarios write their setup logs here; --no-build skips the BUILD block that
# would otherwise create the gitignored dir.
mkdir -p "$TEST_DIR/tmp/out"

# local_sha256 hashes a file with whichever tool the host provides.
local_sha256() {
  if command -v sha256sum >/dev/null; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

if ((!BUILD)); then
  # A stale binary or image makes goldens lie: refuse --no-build when either
  # is missing or older than any Go source, so the failure says "rebuild".
  bin="$TEST_DIR/bin/wtm"
  if [[ ! -x $bin ]]; then
    echo "run.sh: $bin missing; drop --no-build to build it" >&2
    exit 1
  fi
  if [[ -n $(find "$ROOT_DIR" \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$bin" -print -quit) ]]; then
    echo "run.sh: $bin is older than a Go source; drop --no-build to rebuild" >&2
    exit 1
  fi
  # Scenarios run the binary baked into the image, not test/bin/wtm, so the
  # image must also be current: compare hashes, not mtimes.
  command -v docker >/dev/null || { echo "run.sh: docker not found" >&2; exit 1; }
  img_bin=$(docker run --rm --platform "$PLATFORM" --network none \
    --entrypoint sha256sum "$IMAGE" /usr/local/bin/wtm | cut -d' ' -f1) || {
    echo "run.sh: cannot read $IMAGE; drop --no-build to build it" >&2
    exit 1
  }
  if [[ $img_bin != $(local_sha256 "$bin") ]]; then
    echo "run.sh: $IMAGE holds an outdated wtm binary; drop --no-build to rebuild it" >&2
    exit 1
  fi
fi

if ((BUILD)); then
  command -v docker >/dev/null || { echo "run.sh: docker not found" >&2; exit 1; }
  arch=${PLATFORM##*/}
  mkdir -p "$TEST_DIR/bin"
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

docker run --rm --platform "$PLATFORM" --network none \
  -v "$TEST_DIR:/harness" "$IMAGE" bash /harness/manifest_test.sh

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
