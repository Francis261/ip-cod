#!/usr/bin/env bash
# Install the ipcod agent on a VPS (systemd). Run as root after `make build`.
#
# Interactive by default: it prompts for the coordinator URL, agent token,
# display name, and dry-run mode. Any value you also pass as an env var is
# pre-filled as the default, so you can still automate it:
#
#   sudo ./deploy/install-agent.sh
#   sudo COORD=wss://coord.example.com/ws TOKEN=ipca_xxx ./deploy/install-agent.sh
#
# The agent dials OUT to the coordinator (no inbound port needed) and updates
# ufw to allow your laptop's current IP to reach a port you set in the
# dashboard. First run defaults to DRY_RUN=1 (log-only) so you can verify the
# exact ufw rule before it touches the firewall.
set -euo pipefail

# ---------------------------------------------------------------- appearance
if [ -t 1 ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; R=$'\033[0m'
  G=$'\033[32m'; Y=$'\033[33m'; RD=$'\033[31m'; BL=$'\033[34m'
else
  B=''; DIM=''; R=''; G=''; Y=''; RD=''; BL=''
fi
info(){ printf '%s\n' "${BL}→${R} $*"; }
good(){ printf '%s\n' "${G}✓${R} $*"; }
warn(){ printf '%s\n' "${Y}!${R} $*" >&2; }
die(){  printf '%s\n' "${RD}✗${R} $*" >&2; exit 1; }

# Interactive only when stdin is a terminal; otherwise use env/defaults.
INTERACTIVE=0; [ -t 0 ] && INTERACTIVE=1

# prompt "message" "default"  -> sets $REPLY (loops until non-empty)
prompt(){
  local msg="$1" def="${2-}" hint reply
  if [ "$INTERACTIVE" != 1 ]; then REPLY="$def"; return 0; fi
  [ -n "$def" ] && hint=" [$def]"
  while true; do
    printf '%s%s: ' "$msg" "$hint"
    IFS= read -r reply || reply=""
    reply="${reply:-$def}"
    if [ -n "$reply" ]; then REPLY="$reply"; return 0; fi
    printf '%s\n' "  ${DIM}(a value is required)${R}"
  done
}

# confirm "message" y|n  -> sets $REPLY to y or n
confirm(){
  local msg="$1" def="${2:-n}" hint reply
  if [ "$INTERACTIVE" != 1 ]; then REPLY="$def"; return 0; fi
  if [ "$def" = y ]; then hint="Y/n"; else hint="y/N"; fi
  while true; do
    printf '%s [%s]: ' "$msg" "$hint"
    IFS= read -r reply || reply=""
    reply="${reply:-$def}"
    case "$reply" in
      [Yy]*) REPLY=y; return 0 ;;
      [Nn]*) REPLY=n; return 0 ;;
      *) printf '%s\n' "  please answer y or n" ;;
    esac
  done
}

# Turn whatever the user typed into a proper agent WebSocket endpoint.
#   ipcod.devgorge.com      -> wss://ipcod.devgorge.com/ws
#   https://host            -> wss://host/ws
#   wss://host/ws           -> unchanged
normalize_ws(){
  local u="${1%/}"
  case "$u" in
    http://*)  u="ws://${u#http://}" ;;
    https://*) u="wss://${u#https://}" ;;
    ws://*|wss://*) : ;;
    *) u="wss://$u" ;;
  esac
  case "$u" in */ws) : ;; *) u="$u/ws" ;; esac
  printf '%s' "$u"
}

# --------------------------------------------------------------------- checks
[ "$(id -u)" -eq 0 ] || die "must run as root (sudo)"

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_DIR/dist/ipcod-agent"
[ -x "$BIN" ] || die "binary not found at $BIN — run 'make build' first (or download a release binary)"

if ! command -v systemctl >/dev/null 2>&1; then
  die "systemctl not found; this installer targets systemd hosts"
fi

# ------------------------------------------------------------------ questions
printf '\n%s\n' "${B}===========================================${R}"
printf '%s\n'     "${B} ipcod agent installer${R}"
printf '%s\n'     "${B}===========================================${R}"
printf '%s\n\n'   "Installs the agent as a systemd service. It dials out to your${R}
${DIM}coordinator and updates ufw to allow your laptop's IP.${R}"

prompt "Coordinator WebSocket URL (host is fine)" "${COORD:-}"
COORD_RAW="$REPLY"
[ -n "$COORD_RAW" ] || {
  if [ "$INTERACTIVE" = 1 ]; then die "coordinator URL is required"; fi
  die "COORD is required when non-interactive, e.g. COORD=wss://host/ws"
}
COORD="$(normalize_ws "$COORD_RAW")"

prompt "Agent token (${B}ipca_…${R}, from the dashboard)" "${TOKEN:-}"
TOKEN="$REPLY"
[ -n "$TOKEN" ] || {
  if [ "$INTERACTIVE" = 1 ]; then die "agent token is required"; fi
  die "TOKEN is required when non-interactive, e.g. TOKEN=ipca_xxx"
}
case "$TOKEN" in
  ipca_*) : ;;
  ipcb_*) die "that's an ipcb_ BEACON token (for the laptop). The VPS agent needs an ipca_ AGENT token — create one in the dashboard." ;;
  *)      warn "token doesn't start with ipca_; double-check it's the agent token" ;;
esac

prompt "Display name for this VPS" "${NAME:-$(hostname)}"
NAME="$REPLY"

# Safe default: first run logs the ufw command without applying it.
if [ -n "${DRY_RUN:-}" ]; then
  _dr_def="$DRY_RUN"
else
  _dr_def=1
fi
confirm "Dry-run first (log ufw command, don't change the firewall)?" \
  "$([ "$_dr_def" = 1 ] && echo y || echo n)"
[ "$REPLY" = y ] && DRY_RUN=1 || DRY_RUN=0

if [ "$DRY_RUN" != 1 ] && ! command -v ufw >/dev/null 2>&1; then
  warn "ufw not found on PATH; the agent won't be able to apply rules."
  confirm "Continue anyway?" n
  [ "$REPLY" = y ] || die "aborted — install ufw, or re-run and choose dry-run"
fi

# -------------------------------------------------------------------- summary
if [ "$INTERACTIVE" = 1 ]; then
  printf '\n%s\n' "${B}── Summary${R}"
  printf '  coordinator : %s\n' "$COORD"
  printf '  token       : %s…\n' "${TOKEN:0:9}"
  printf '  name        : %s\n' "$NAME"
  if [ "$DRY_RUN" = 1 ]; then
    printf '  mode        : %sDRY-RUN%s (log only — nothing applied)\n' "$Y" "$R"
  else
    printf '  mode        : %sLIVE%s (will manage ufw)\n' "$RD" "$R"
  fi
  printf '  binary      : /usr/local/bin/ipcod-agent\n'
  printf '  unit        : /etc/systemd/system/ipcod-agent.service\n\n'
  confirm "Proceed with installation?" y
  [ "$REPLY" = y ] || die "aborted"
fi

# --------------------------------------------------------------------- install
info "installing binary -> /usr/local/bin/ipcod-agent"
install -m 0755 "$BIN" /usr/local/bin/ipcod-agent

info "writing /etc/ipcod/agent.env"
mkdir -p /etc/ipcod
cat > /etc/ipcod/agent.env <<EOF
# ipcod agent configuration — written by install-coordinator/install-agent.sh
# Variable names are prefixed IPCOD_ and MUST match exactly.

# Coordinator WebSocket endpoint.
IPCOD_COORDINATOR=$COORD

# Agent bearer token (ipca_…).
IPCOD_TOKEN=$TOKEN

# Display name in the dashboard.
IPCOD_NAME=$NAME

# ufw binary path.
IPCOD_UFW=ufw

# 1 = log ufw commands without applying; 0 = manage the firewall.
IPCOD_DRY_RUN=$DRY_RUN
EOF
chmod 0600 /etc/ipcod/agent.env
good "wrote /etc/ipcod/agent.env (root:root, 0600)"

info "installing systemd unit + starting"
install -m 0644 "$REPO_DIR/deploy/ipcod-agent.service" /etc/systemd/system/ipcod-agent.service
systemctl daemon-reload
systemctl enable --now ipcod-agent >/dev/null 2>&1 || true

# Wait briefly, then report liveness from the unit's own perspective.
sleep 1
if systemctl is-active --quiet ipcod-agent; then
  good "ipcod-agent service is active"
else
  warn "service isn't active yet — check: journalctl -u ipcod-agent -e"
fi

# ----------------------------------------------------------------------- done
printf '\n%s\n' "${B}── Installed. What's next${R}"
printf '  status  : %ssystemctl status ipcod-agent%s\n' "$DIM" "$R"
printf '  logs    : %sjournalctl -u ipcod-agent -f%s\n' "$DIM" "$R"
printf '  config  : %s/etc/ipcod/agent.env%s\n' "$DIM" "$R"
if [ "$DRY_RUN" = 1 ]; then
  printf '\n  %sDry-run is ON.%s Watch the logs for the exact rule, e.g.\n' "$Y" "$R"
  printf '    %sufw allow from <your-laptop-ip> to any port <port> proto tcp comment ipcod%s\n' "$DIM" "$R"
  printf '  When it looks right, go live:\n'
  printf '    %ssudo sed -i "s/^IPCOD_DRY_RUN=.*/IPCOD_DRY_RUN=0/" /etc/ipcod/agent.env%s\n' "$DIM" "$R"
  printf '    %ssudo systemctl restart ipcod-agent%s\n' "$DIM" "$R"
fi
printf '\n  Verify it shows %sconnected%s in the coordinator dashboard.\n\n' "$G" "$R"
