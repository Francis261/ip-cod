#!/usr/bin/env bash
# Install the ipcod coordinator on this host (systemd, TLS via reverse proxy
# is your responsibility — see README). Run as root from the repo root after
# `make build`.
#
#   sudo ./deploy/install-coordinator.sh
#
# Environment overrides:
#   ADMIN_TOKEN   admin bearer token (generated if unset)
#   LISTEN        default :8443
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (sudo)" >&2
  exit 1
fi

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_DIR/dist/ipcod-coordinator"

if [ ! -x "$BIN" ]; then
  echo "binary not found at $BIN — run 'make build' first" >&2
  exit 1
fi

LISTEN="${LISTEN:-:8443}"
if [ -z "${ADMIN_TOKEN:-}" ]; then
  ADMIN_TOKEN="$(openssl rand -base64 32)"
  echo "generated ADMIN_TOKEN (save this):"
  echo "  $ADMIN_TOKEN"
fi

echo "installing coordinator binary -> /usr/local/bin/ipcod-coordinator"
install -m 0755 "$BIN" /usr/local/bin/ipcod-coordinator

echo "installing env -> /etc/ipcod/coordinator.env"
mkdir -p /etc/ipcod
if [ ! -f /etc/ipcod/coordinator.env ]; then
  sed -e "s|^LISTEN=.*|LISTEN=$LISTEN|" \
      -e "s|^ADMIN_TOKEN=.*|ADMIN_TOKEN=$ADMIN_TOKEN|" \
      "$REPO_DIR/deploy/coordinator.env.example" > /etc/ipcod/coordinator.env
  chmod 0600 /etc/ipcod/coordinator.env
else
  echo "  /etc/ipcod/coordinator.env already exists; leaving it untouched"
fi

echo "installing systemd unit"
install -m 0644 "$REPO_DIR/deploy/ipcod-coordinator.service" /etc/systemd/system/ipcod-coordinator.service
systemctl daemon-reload
systemctl enable --now ipcod-coordinator

echo
echo "coordinator installed."
echo "  status : systemctl status ipcod-coordinator"
echo "  logs   : journalctl -u ipcod-coordinator -f"
echo "  health : curl -s http://127.0.0.1:8443/healthz"
echo
echo "REMEMBER: put a TLS reverse proxy in front (wss:// for agents). See README."
