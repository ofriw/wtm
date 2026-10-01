# wtm container test harness: Debian + git + real chunkhound 6.0.0.
#
# Scenarios must stay offline. /stub-bin/chunkhound is first on PATH and
# injects --no-embeddings into every call, so no scenario ever reaches an
# embedding provider. Network is only needed at image build time (uv + wheel).
#
# The base digest AND the git package version are pinned: goldens hash git hook
# samples and worktree admin bytes, so a silently upgraded git would fail every
# scenario confusingly. Version bumps must flow through the golden.yml regen PR
# workflow.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl diffutils findutils git=1:2.39.5-0+deb12u3 python3 \
 && rm -rf /var/lib/apt/lists/*

# uv provisions a managed Python for the pinned chunkhound tool env.
RUN curl -LsSf https://astral.sh/uv/install.sh | sh \
 && /root/.local/bin/uv tool install chunkhound==6.0.0

# Warm DuckDB's extension cache so scenarios can run with --network none.
# chunkhound INSTALLs the `vss` extension on every connect; the stub pins
# chunkhound's HOME to /root, so this cache is found without network access.
RUN mkdir -p /tmp/warm \
 && git -C /tmp/warm init -q -b main \
 && printf 'def warm(): return 1\n' >/tmp/warm/warm.py \
 && git -C /tmp/warm add -A \
 && git -C /tmp/warm -c user.name=wtm -c user.email=wtm@example.com commit -qm warm \
 && HOME=/root CHUNKHOUND_NO_PROMPTS=1 /root/.local/bin/chunkhound index /tmp/warm --no-embeddings \
 && rm -rf /tmp/warm

COPY stub-bin/chunkhound /stub-bin/chunkhound
COPY bin/wtm /usr/local/bin/wtm
RUN chmod +x /stub-bin/chunkhound /usr/local/bin/wtm
