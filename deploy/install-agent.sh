#!/usr/bin/env bash
# Install the ipcod agent on a VPS (systemd). Run as root from the repo root
# after `make build`. Requires ufw installed and the agent token from the
# coordinator dashboard.
#
#   sudo COORD=wss://coord.example.com/ws TOKEN=ipca_xxx ./deploy/install-agent.sh
#
# Optional:
#   NAME       display name (defaults to hostname)
#   DRY_RUN=1  log ufw commands instead of applying (safe test)
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (sudo)" >&2
  exit 1
fi

COORD="${COORD:-}"
TOKEN="${TOKEN:-}"
NAME="${NAME:-$(hostname)}"
DRY_RUN="${DRY_RUN:-0}"

if [ -z "$COORD" ] || [ -z "$TOKEN" ]; then
  echo "COORD and TOKEN are required, e.g.:" >&2
  echo "  sudo COORD=wss://coord.example.com/ws TOKEN=ipca_xxx $0" >&2
  exit 1
fi

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_DIR/dist/ipcod-agent"

if [ ! -x "$BIN" ]; then
  echo "binary not found at $BIN — run 'make build' first" >&2
  exit 1
fi

if ! command -v ufw >/dev/null 2>&1 && [ "$DRY_RUN" != "1" ]; then
  echo "warning: ufw not found on PATH; agent will fail to apply rules." >&2
  echo "         install ufw, or set DRY_RUN=1 to test connectivity only." >&2
fi

echo "installing agent binary -> /usr/local/bin/ipcod-agent"
install -m 0755 "$BIN" /usr/local/bin/ipcod-agent

echo "writing /etc/ipcod/agent.env"
mkdir -p /etc/ipcod
cat > /etc/ipcod/agent.env <<EOF
COORDINATOR=$COORD
TOKEN=$TOKEN
NAME=$NAME
UFW_BIN=ufw
DRY_RUN=$DRY_RUN
EOF
chmod 0600 /etc/ipcod/agent.env

echo "installing systemd unit"
install -m 0644 "$REPO_DIR/deploy/ipcod-agent.service" /etc/systemd/system/ipcod-agent.service
systemctl daemon-reload
systemctl enable --now ipcod-agent

echo
echo "agent installed."
echo "  status : systemctl status ipcod-agent"
echo "  logs   : journalctl -u ipcod-agent -f"
echo
echo "Verify it shows up as 'connected' in the coordinator dashboard."
