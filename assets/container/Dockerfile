# Image for the container sandbox: Claude Code as a non-root user, built locally so no registry login is needed.
# The entrypoint starts as root only to give the node user the host uid, then drops to it.
FROM node:22-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git procps ripgrep \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code && npm cache clean --force
COPY entrypoint.sh /usr/local/bin/agent-relay-entrypoint
RUN chmod 755 /usr/local/bin/agent-relay-entrypoint
WORKDIR /workspace
ENTRYPOINT ["agent-relay-entrypoint"]
