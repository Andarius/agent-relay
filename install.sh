#!/usr/bin/env bash
# DESCRIPTION: install agent-relay for this machine (checksum-verified), then start its guided setup
# USAGE:
#   curl -fsSL https://github.com/Andarius/agent-relay/releases/latest/download/install.sh | bash
#   bash install.sh   # from an unpacked release folder
# EXAMPLES:
#   … | AGENT_RELAY_DIR=/usr/local/bin bash
#   … | AGENT_RELAY_NO_INIT=1 bash
set -euo pipefail

repo=${AGENT_RELAY_REPO:-Andarius/agent-relay}
dest=${AGENT_RELAY_DIR:-$HOME/.local/bin}
here=$(cd "$(dirname "${BASH_SOURCE[0]:-.}")" && pwd)
from=${AGENT_RELAY_FROM:-}
[[ -z $from && -f $here/SHA256SUMS ]] && from=$here

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac
case $os in linux | darwin) ;; *) echo "unsupported OS: $os (Windows: download the .exe from the release)" >&2; exit 1 ;; esac
bin="agent-relay-$os-$arch"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# fetch NAME copies one release file into $tmp
fetch() {
  if [[ -n $from ]]; then
    cp "$from/$1" "$tmp/$1"
  elif [[ -n ${AGENT_RELAY_BASE:-} ]]; then
    curl -fsSL "$AGENT_RELAY_BASE/$1" -o "$tmp/$1"
  elif command -v gh >/dev/null && gh auth status >/dev/null 2>&1; then
    gh release download -R "$repo" -p "$1" -D "$tmp" --clobber
  else
    curl -fsSL "https://github.com/$repo/releases/latest/download/$1" -o "$tmp/$1" || {
      echo "Download failed: could not fetch $1 from the latest $repo release." >&2
      exit 1
    }
  fi
}

echo "Fetching $bin${from:+ from $from}…"
fetch "$bin"
fetch SHA256SUMS
want=$(awk -v f="$bin" '$2 == f {print $1}' "$tmp/SHA256SUMS")
if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$bin" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp/$bin" | cut -d' ' -f1); fi
[[ -n $want && $want == "$got" ]] || { echo "checksum mismatch for $bin" >&2; exit 1; }

mkdir -p "$dest"
install -m 755 "$tmp/$bin" "$dest/agent-relay"
echo "Installed $dest/agent-relay ($("$dest/agent-relay" version))"
[[ :$PATH: == *":$dest:"* ]] || echo "Add $dest to your PATH to run agent-relay from anywhere."

[[ -n ${AGENT_RELAY_NO_INIT:-} ]] && exit 0
# stdin may be the download pipe; the wizard needs the terminal
exec "$dest/agent-relay" init </dev/tty
