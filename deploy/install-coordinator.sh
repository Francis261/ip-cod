#!/usr/bin/env bash
# Install the ipcod coordinator on this host (systemd). TLS termination is
# your responsibility — a reverse proxy or Cloudflare tunnel in front; see
# README. Run as root after `make build`.
#
# Interactive by default: prompts for the listen port, admin token, and
# whether you're behind a proxy/tunnel. Env vars still work for automation:
#
#   sudo ./deploy/install-coordinator.sh
#   sudo LISTEN=:8443 ADMIN_TOKEN=... TRUST_PROXY=1 ./deploy/install-coordinator.sh
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

INTERACTIVE=0; [ -t 0 ] && INTERACTIVE=1

prompt(){   # prompt "message" "default" -> $REPLY
  local msg="$1" def="${2-}" hint="" reply=""
  if [ "$INTERACTIVE" != 1 ]; then REPLY="$def"; return 0; fi
  [ -n "$def" ] && hint=" [$def]"
  while true; do
    printf '%s%s: ' "$msg" "$hint"
    if ! IFS= read -r reply; then printf '\n'; die "aborted — end of input"; fi
    reply="${reply:-$def}"
    if [ -n "$reply" ]; then REPLY="$reply"; return 0; fi
    printf '%s\n' "  ${DIM}(a value is required)${R}"
  done
}

confirm(){  # confirm "message" y|n -> $REPLY
  local msg="$1" def="${2:-n}" hint="" reply=""
  if [ "$INTERACTIVE" != 1 ]; then REPLY="$def"; return 0; fi
  if [ "$def" = y ]; then hint="Y/n"; else hint="y/N"; fi
  while true; do
    printf '%s [%s]: ' "$msg" "$hint"
    if ! IFS= read -r reply; then printf '\n'; die "aborted — end of input"; fi
    reply="${reply:-$def}"
    case "$reply" in
      [Yy]*) REPLY=y; return 0 ;;
      [Nn]*) REPLY=n; return 0 ;;
      *) printf '%s\n' "  please answer y or n" ;;
    esac
  done
}

# Accept ":8443", "8443", or "0.0.0.0:8443" -> canonical ":port" / "host:port".
normalize_listen(){
  local l="$1"
  case "$l" in
    :*)        : ;;               # already :port
    *:*)       : ;;               # host:port
    *)         l=":$l" ;;         # bare port
  esac
  printf '%s' "$l"
}

# --------------------------------------------------------------------- checks
[ "$(id -u)" -eq 0 ] || die "must run as root (sudo)"

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_DIR/dist/ipcod-coordinator"
[ -x "$BIN" ] || die "binary not found at $BIN — run 'make build' first (or download a release binary)"
command -v systemctl >/dev/null 2>&1 || die "systemctl not found; this installer targets systemd hosts"

ENV_FILE=/etc/ipcod/coordinator.env

# ------------------------------------------------------------------ questions
printf '\n%s\n' "${B}===============================================${R}"
printf '%s\n'     "${B} ipcod coordinator installer${R}"
printf '%s\n'     "${B}===============================================${R}"
printf '%s\n\n'   "Installs the coordinator as a systemd service. Put a TLS${R}
${DIM}reverse proxy or Cloudflare tunnel in front so agents use wss://.${R}"

prompt "Listen address" "${LISTEN:-:8443}"
LISTEN="$(normalize_listen "$REPLY")"

# Admin token: generate one unless already provided.
if [ -n "${ADMIN_TOKEN:-}" ]; then
  info "using ADMIN_TOKEN from environment"
else
  confirm "Generate a new admin bearer token?" y
  if [ "$REPLY" = y ]; then
    ADMIN_TOKEN="$(openssl rand -base64 32 2>/dev/null || head -c 32 /dev/urandom | base64)"
    printf '\n  %sAdmin token (save this now — it guards the dashboard & API):%s\n' "$Y" "$R"
    printf '  %s%s%s\n\n' "$B" "$ADMIN_TOKEN" "$R"
  else
    prompt "Paste your admin token" ""
    ADMIN_TOKEN="$REPLY"
    [ -n "$ADMIN_TOKEN" ] || die "admin token is required"
  fi
fi

# Trust proxy: only relevant for legacy beacon check-ins (POST /api/beacon);
# the coordinator's own outbound IP detection ignores it entirely.
confirm "Running behind a reverse proxy or Cloudflare tunnel (trust X-Forwarded-For)?" "${TRUST_PROXY:-n}"
[ "$REPLY" = y ] && TRUST_PROXY=1 || TRUST_PROXY=0

if [ "$TRUST_PROXY" = 1 ]; then
  good "TRUST_PROXY=1 — legacy beacon check-ins will take the client IP from X-Forwarded-For"
else
  printf '  %sTRUST_PROXY=0 — fine unless you still use the legacy beacon endpoint\n' "$Y"
  printf '  %sbehind a proxy, in which case its check-ins get the proxy IP.%s\n' "$Y" "$R"
fi

# Web dashboard: run as `web -p <port>` instead of headless `run`. WEB=y/n
# forces the choice (automation / manage.sh); otherwise ask, defaulting to
# whatever is already configured (WEB_PORT non-empty = currently on).
if [ -n "${WEB:-}" ]; then
  case "$WEB" in [Yy]*|1) WEB=y ;; *) WEB=n ;; esac
else
  confirm "Serve the web dashboard (admin UI in a browser)?" \
    "$([ -n "${WEB_PORT:-}" ] && echo y || echo n)"
  [ "$REPLY" = y ] && WEB=y || WEB=n
fi
if [ "$WEB" = y ]; then
  if [ -n "${WEB_PORT:-}" ]; then
    info "dashboard port from environment: $WEB_PORT"
  else
    prompt "Dashboard port" "8443"
    WEB_PORT="$REPLY"
  fi
  case "$WEB_PORT" in ''|*[!0-9]*) die "dashboard port must be a number (got '$WEB_PORT')" ;; esac
  [ "$WEB_PORT" -ge 1 ] && [ "$WEB_PORT" -le 65535 ] || die "dashboard port must be between 1 and 65535"
  good "web dashboard: http://127.0.0.1:$WEB_PORT/ (same API, plus the UI)"
else
  WEB_PORT=""
fi

# -------------------------------------------------------------------- summary
if [ "$INTERACTIVE" = 1 ]; then
  printf '\n%s\n' "${B}── Summary${R}"
  printf '  listen      : %s\n' "$LISTEN"
  printf '  admin token : %s…\n' "${ADMIN_TOKEN:0:9}"
  printf '  trust-proxy : %s\n' "$TRUST_PROXY"
  printf '  web ui      : %s\n' "$([ "$WEB" = y ] && echo "on (port $WEB_PORT)" || echo "off — start later with: ipcod-coordinator web -p 8443")"
  printf '  detect ip   : every 60s via public echo services (no beacon needed)\n'
  printf '  db          : /var/lib/ipcod/ipcod.db\n'
  printf '  binary      : /usr/local/bin/ipcod-coordinator\n'
  printf '  unit        : /etc/systemd/system/ipcod-coordinator.service\n\n'
  confirm "Proceed with installation?" y
  [ "$REPLY" = y ] || die "aborted"
fi

# --------------------------------------------------------------------- install
info "installing binary -> /usr/local/bin/ipcod-coordinator"
install -m 0755 "$BIN" /usr/local/bin/ipcod-coordinator

info "writing $ENV_FILE"
mkdir -p /etc/ipcod
# systemd's DynamicUser + StateDirectory=ipcod must own /var/lib/ipcod itself
# (it materialises as a symlink into /var/lib/private). Pre-creating the
# directory as root — or leaving one behind — makes the unit die at startup
# with status=238/STATE_DIRECTORY. So clear an empty leftover, and move a
# non-empty one aside instead of silently fighting systemd for it.
if [ -d /var/lib/ipcod ] && [ ! -L /var/lib/ipcod ]; then
  if [ -n "$(ls -A /var/lib/ipcod 2>/dev/null)" ]; then
    BAK="/var/lib/ipcod.bak.$(date +%Y%m%d%H%M%S)"
    mv /var/lib/ipcod "$BAK"
    good "moved existing /var/lib/ipcod aside -> $BAK (move the .db back if you need that data)"
  else
    rmdir /var/lib/ipcod
  fi
fi
if [ -f "$ENV_FILE" ]; then
  cp -a "$ENV_FILE" "$ENV_FILE.bak.$(date +%Y%m%d%H%M%S)"
  good "backed up existing env to $ENV_FILE.bak.*"
fi
cat > "$ENV_FILE" <<EOF
# ipcod coordinator configuration — written by install-coordinator.sh
# Variable names are prefixed IPCOD_ and MUST match exactly.

# HTTP listen address.
IPCOD_LISTEN=$LISTEN

# Admin bearer token guarding /api/admin/* and the dashboard login.
IPCOD_ADMIN_TOKEN=$ADMIN_TOKEN

# 1 = trust X-Forwarded-For (behind a reverse proxy / Cloudflare tunnel);
# 0 = use the direct socket address (only affects legacy beacon check-ins).
IPCOD_TRUST_PROXY=$TRUST_PROXY

# Outbound public-IP detection (the replacement for the old beacon process).
# Defaults are built into the binary; uncomment to override.
#IPCOD_DETECT_INTERVAL=60s
#IPCOD_IP_PROVIDERS=https://api.ipify.org,https://icanhazip.com
#IPCOD_NO_DETECT=0
EOF
chmod 0600 "$ENV_FILE"
good "wrote $ENV_FILE (root:root, 0600)"

info "installing systemd unit + starting"
install -m 0644 "$REPO_DIR/deploy/ipcod-coordinator.service" /etc/systemd/system/ipcod-coordinator.service

# Swap ExecStart to `web -p <port>` when the dashboard was requested; remove a
# stale drop-in otherwise so re-running the installer returns to headless.
DROPIN_DIR=/etc/systemd/system/ipcod-coordinator.service.d
if [ "$WEB" = y ]; then
  info "enabling the web dashboard on port $WEB_PORT (unit drop-in)"
  mkdir -p "$DROPIN_DIR"
  cat > "$DROPIN_DIR/web.conf" <<EOF
# Written by install-coordinator.sh: run the coordinator as \`web -p $WEB_PORT\`.
[Service]
ExecStart=
ExecStart=/usr/local/bin/ipcod-coordinator web -p $WEB_PORT
EOF
elif [ -f "$DROPIN_DIR/web.conf" ]; then
  rm -f "$DROPIN_DIR/web.conf"
  rmdir "$DROPIN_DIR" 2>/dev/null || true
  info "web dashboard disabled (removed the drop-in)"
fi

systemctl daemon-reload
if systemctl is-active --quiet ipcod-coordinator; then
  # Already running: restart so the new env (and drop-in) actually take effect.
  systemctl restart ipcod-coordinator >/dev/null 2>&1 || true
else
  systemctl enable --now ipcod-coordinator >/dev/null 2>&1 || true
fi

# ------------------------------------------------------------------ healthz
sleep 1
# Derive a loopback URL regardless of how LISTEN is spelled:
# ":8443", "127.0.0.1:8443", "0.0.0.0:8443", "[::1]:8443".
PORT="${LISTEN##*:}"
case "$PORT" in ''|*[!0-9]*) PORT=8443 ;; esac
LOOP="http://127.0.0.1:${PORT}"
ok=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if curl -sf --max-time 2 "$LOOP/healthz" >/dev/null 2>&1; then ok=1; break; fi
  sleep 0.5
done
if systemctl is-active --quiet ipcod-coordinator && [ "$ok" = 1 ]; then
  good "coordinator healthy at $LOOP/healthz"
elif systemctl is-active --quiet ipcod-coordinator; then
  warn "service active but healthz not answering yet — check: journalctl -u ipcod-coordinator -e"
else
  warn "service isn't active — check: journalctl -u ipcod-coordinator -e"
fi

# ----------------------------------------------------------------------- done
printf '\n%s\n' "${B}── Installed. What's next${R}"
printf '  status  : %ssystemctl status ipcod-coordinator%s\n' "$DIM" "$R"
printf '  logs    : %sjournalctl -u ipcod-coordinator -f%s\n' "$DIM" "$R"
printf '  health  : %scurl -s %s/healthz%s\n' "$DIM" "$LOOP" "$R"
printf '  config  : %s%s%s\n' "$DIM" "$ENV_FILE" "$R"
if [ "$WEB" = y ]; then
  printf '  web ui  : open http://127.0.0.1:%s/ (or http://<host>:%s/ remotely)\n' "$WEB_PORT" "$WEB_PORT"
else
  printf '  web ui  : %s ipcod-coordinator web -p 8443%s to serve the dashboard\n' "$DIM" "$R"
fi
printf '\n  %sIMPORTANT:%s point agents at wss://<your-host>/ws — put a TLS reverse\n' "$RD" "$R"
printf '  proxy (Caddy/nginx) or a Cloudflare tunnel in front so wss:// works.\n'
printf '  The coordinator learns the public IP on its own; nothing else\n'
printf '  needs to run on this machine.\n\n'
