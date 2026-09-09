#!/bin/sh
# FivePanel agent installer for Linux (amd64 / arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/5panel/agent/main/install.sh | sudo sh
#   curl -fsSL https://raw.githubusercontent.com/5panel/agent/main/install.sh | \
#     sudo sh -s -- --token fp_live_... --db "mysql://fivepanel_ro:pass@127.0.0.1:3306/qbcore"
#
# What it does: downloads the release archive for this machine's
# architecture from GitHub Releases, verifies its checksum, installs the
# binary to /usr/local/bin, runs `fivepanel-agent setup` when --token and
# --db are given, and installs + starts the systemd service. Nothing else
# on the system is touched. Read it before you run it.
set -eu

REPO="5panel/agent"
BIN_DIR="/usr/local/bin"
CONFIG="/etc/fivepanel/config.yaml"
VERSION="${FIVEPANEL_AGENT_VERSION:-latest}"
TOKEN=""
DB=""
ENGINE=""
GATEWAY=""
NO_SERVICE=0

usage() {
  cat <<EOF
Usage: install.sh [--token TOKEN] [--db URL] [--engine mysql|mongodb] [--gateway URL] [--version vX.Y.Z] [--no-service]

With --token and --db the configuration is written and the service started.
Without them only the binary is installed; run 'sudo fivepanel-agent setup' next.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --db) DB="$2"; shift 2 ;;
    --engine) ENGINE="$2"; shift 2 ;;
    --gateway) GATEWAY="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --no-service) NO_SERVICE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root (sudo): the installer writes to $BIN_DIR and /etc/fivepanel" >&2
  exit 1
fi

case "$(uname -s)" in
  Linux) OS=linux ;;
  *) echo "this installer supports Linux only; see README.md for other systems" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

ARCHIVE="fivepanel-agent_${OS}_${ARCH}.tar.gz"
if [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$1" -O "$2"
  else
    echo "curl or wget is required" >&2
    exit 1
  fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "downloading $BASE/$ARCHIVE"
fetch "$BASE/$ARCHIVE" "$TMP/$ARCHIVE"
fetch "$BASE/checksums.txt" "$TMP/checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$TMP" && grep " $ARCHIVE\$" checksums.txt | sha256sum -c - >/dev/null) || {
    echo "checksum mismatch for $ARCHIVE" >&2
    exit 1
  }
  echo "checksum ok"
else
  echo "sha256sum not found; skipping checksum verification" >&2
fi

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" fivepanel-agent
install -m 0755 "$TMP/fivepanel-agent" "$BIN_DIR/fivepanel-agent"
echo "installed $BIN_DIR/fivepanel-agent ($("$BIN_DIR/fivepanel-agent" version))"

if [ -n "$TOKEN" ] && [ -n "$DB" ]; then
  set -- --token "$TOKEN" --db "$DB" --config "$CONFIG" --yes
  [ -n "$ENGINE" ] && set -- "$@" --engine "$ENGINE"
  [ -n "$GATEWAY" ] && set -- "$@" --gateway "$GATEWAY"
  "$BIN_DIR/fivepanel-agent" setup "$@"
elif [ ! -f "$CONFIG" ]; then
  echo
  echo "next: sudo fivepanel-agent setup"
  exit 0
fi

if [ "$NO_SERVICE" -eq 0 ] && [ -f "$CONFIG" ]; then
  if "$BIN_DIR/fivepanel-agent" service status --config "$CONFIG" 2>/dev/null | grep -q "not installed"; then
    "$BIN_DIR/fivepanel-agent" service install --config "$CONFIG"
    "$BIN_DIR/fivepanel-agent" service start
  else
    "$BIN_DIR/fivepanel-agent" service restart --config "$CONFIG" || "$BIN_DIR/fivepanel-agent" service start
  fi
  "$BIN_DIR/fivepanel-agent" service status
  echo "logs: journalctl -u fivepanel-agent -f"
fi
