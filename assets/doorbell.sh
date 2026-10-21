#!/usr/bin/env bash
# DESCRIPTION: print one line per relay doorbell file so a Claude Monitor wakes the agent; bodies stay in the relay
# USAGE: doorbell.sh [INBOX_DIR]   (default: ./.agent-inbox)
set -euo pipefail
shopt -s nullglob
dir=${1:-.agent-inbox}
mkdir -p "$dir"
while true; do
  for f in "$dir"/*.json; do
    echo "relay: new message $(basename "$f" .json), call read_inbox"
    rm -f -- "$f"
  done
  sleep 2
done
