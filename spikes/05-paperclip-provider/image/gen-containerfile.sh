#!/usr/bin/env bash
# Prints the spike 05 agent image. The wsm build context holds only the
# Containerfile (ADR 0008), so the mock model and the wrapper come in as
# heredocs. Usage: gen-containerfile.sh <node-version, e.g. v24.11.1>
set -euo pipefail
NODE_VERSION=$1
HERE=$(cd "$(dirname "$0")" && pwd)
cat <<EOF
# Spike 05 agent image: Node ${NODE_VERSION}, Claude Code and the scripted
# mock model, on top of plan section 3.8 (user 1000, home /home/agent).
FROM docker.io/library/debian:trixie-slim
RUN apt-get update \\
 && apt-get install -y --no-install-recommends bash tmux git ca-certificates curl xz-utils \\
 && rm -rf /var/lib/apt/lists/* \\
 && cd /tmp \\
 && curl -fsSLO https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-x64.tar.xz \\
 && curl -fsSLO https://nodejs.org/dist/${NODE_VERSION}/SHASUMS256.txt \\
 && grep " node-${NODE_VERSION}-linux-x64.tar.xz\\\$" SHASUMS256.txt | sha256sum -c - \\
 && tar -xJf node-${NODE_VERSION}-linux-x64.tar.xz -C /usr/local --strip-components=1 --no-same-owner \\
 && rm -f node-${NODE_VERSION}-linux-x64.tar.xz SHASUMS256.txt \\
 && npm install -g @anthropic-ai/claude-code@2.1.330 \\
 && npm cache clean --force \\
 && groupadd --gid 1000 agent \\
 && useradd --uid 1000 --gid 1000 --home-dir /home/agent --create-home --shell /bin/bash agent \\
 && install -d -o 1000 -g 1000 /workspace
RUN cat > /usr/local/lib/mockllm.mjs <<'EOF_MOCK'
$(cat "$HERE/mockllm.mjs")
EOF_MOCK
RUN chmod 0644 /usr/local/lib/mockllm.mjs
WORKDIR /workspace
# wsm keeps the image CMD (ADR 0007): the mock model starts with the container.
CMD ["sh", "-c", "MOCK_ADDR=127.0.0.1:8787 node /usr/local/lib/mockllm.mjs >>/tmp/mockllm.log 2>&1 & exec sleep infinity"]
EOF
