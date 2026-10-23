#!/bin/sh
# DESCRIPTION: start Claude as the node user with the host uid and the relay's MCP endpoint
# The MCP URL holds the token, so it comes in through the environment, never argv.
set -eu
uid=${AGENT_RELAY_UID:-1000}
[ "$(id -u node)" = "$uid" ] || usermod -o -u "$uid" node
[ "$(stat -c %u /home/node)" = "$uid" ] || chown -R node /home/node
cfg=$(mktemp)
printf '{"mcpServers":{"agent-relay":{"type":"http","url":"%s"}}}\n' "$AGENT_RELAY_MCP_URL" > "$cfg"
chown node "$cfg"
unset AGENT_RELAY_MCP_URL AGENT_RELAY_UID
# --mcp-config takes every argument after it, so the caller's arguments come first.
exec setpriv --reuid=node --regid=node --init-groups env HOME=/home/node claude "$@" --mcp-config "$cfg"
