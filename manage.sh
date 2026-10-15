#!/usr/bin/env bash
#
# manage.sh — interactive lifecycle manager for ipcod.
#
# One script to set up and operate all three roles:
#
#   coordinator  the IP-push server (systemd: ipcod-coordinator)
#   agent        the per-VPS firewall daemon (systemd: ipcod-agent)
#   beacon       the laptop IP reporter (systemd --user: ipcod-beacon)
#
# Interactive (menu-driven) by default:
#
#   ./manage.sh
#
# Scriptable: every menu action is also a subcommand:
#
#   ./manage.sh status
#   sudo ./manage.sh setup coordinator
#   ./manage.sh restart beacon
#   ./manage.sh logs agent --follow
#   ./manage.sh create-agent web-1 22
#   ./manage.sh ip 203.0.113.7
#   ./manage.sh help
#
# On a systemd host it drives the real services (and delegates setup to the
# interactive deploy/install-*.sh installers). Without systemd it falls back
# to plain background processes with pidfiles + logs under
# ${XDG_STATE_HOME:-~/.local/state}/ipcod, which is handy for local testing.
set -euo pipefail

# --------------------------------------------------------------------- layout
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST="$REPO_DIR/dist"
DEPLOY="$REPO_DIR/deploy"

COMPONENTS="coordinator agent beacon"

COORD_UNIT=ipcod-coordinator
AGENT_UNIT=ipcod-agent
BEACON_UNIT=ipcod-beacon

COORD_ENV=/etc/ipcod/coordinator.env
AGENT_ENV=/etc/ipcod/agent.env
BEACON_ENV="${XDG_CONFIG_HOME:-$HOME/.config}/ipcod-beacon.env"

COORD_BIN=/usr/local/bin/ipcod-coordinator
AGENT_BIN=/usr/local/bin/ipcod-agent
BEACON_BIN="$HOME/.local/bin/ipcod-beacon"

STATE_DIR="${IPCOD_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/ipcod}"
LOG_DIR="$STATE_DIR/logs"

EDITOR_BIN="${EDITOR:-vi}"

# ----------------------------------------------------------------- appearance
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

banner(){
  printf '\n%s\n' "${B}════════════════════════════════════════${R}"
  printf '%s\n'   "${B} ipcod manage — coordinator/agent/beacon${R}"
  printf '%s\n\n' "${B}════════════════════════════════════════${R}"
}

# Interactive only when stdin is a terminal; otherwise use env/defaults.
INTERACTIVE=0; [ -t 0 ] && INTERACTIVE=1
YES=0; case "${IPCOD_YES:-}" in 1|y|Y|yes) YES=1 ;; esac   # -y / --yes: pre-answer confirms with y

prompt(){   # prompt "message" "default" -> $REPLY (loops until non-empty)
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

confirm(){  # confirm "message" y|n -> $REPLY (honours -y/--yes)
  local msg="$1" def="${2:-n}" hint reply
  [ "$YES" = 1 ] && def=y
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

pick_comp(){ # [coordinator/agent/beacon] -> $REPLY (prompts when omitted)
  local r="${1:-}"
  case "$r" in
    co*|c|1) REPLY=coordinator; return 0 ;;
    a*|2)    REPLY=agent;       return 0 ;;
    b*|3)    REPLY=beacon;      return 0 ;;
  esac
  if [ "$INTERACTIVE" = 1 ]; then
    printf '  %s1)%s coordinator   %s2)%s agent   %s3)%s beacon\n' "$DIM" "$R" "$DIM" "$R" "$DIM" "$R"
  fi
  while true; do
    [ "$INTERACTIVE" = 1 ] && printf 'Component: '
    IFS= read -r r || r="${1:-}"
    case "$r" in
      co*|c|1) REPLY=coordinator; return 0 ;;
      a*|2)    REPLY=agent;       return 0 ;;
      b*|3)    REPLY=beacon;      return 0 ;;
      *) [ "$INTERACTIVE" = 1 ] && printf '%s\n' "  answer coordinator, agent or beacon" ;;
    esac
    [ "$INTERACTIVE" = 1 ] || die "component must be coordinator, agent or beacon"
  done
}

# ---------------------------------------------------------------- tiny tools
command_ok(){ command -v "$1" >/dev/null 2>&1; }
have_jq(){ command_ok jq; }
have_py(){ command_ok python3; }

yn(){ case "${1:-}" in 1|y|Y|yes|true) printf 'y' ;; *) printf 'n' ;; esac; }

systemd_ok(){ command_ok systemctl && [ -d /run/systemd/system ]; }
user_systemd_ok(){ systemd_ok && systemctl --user show-environment >/dev/null 2>&1; }

is_root(){ [ "$(id -u)" -eq 0 ]; }

as_root(){ # run a command as root, using sudo only when needed
  if is_root; then "$@"; else sudo "$@"; fi
}

json_pp(){ # pretty-print JSON on stdin (best effort)
  if have_jq; then jq .
  elif have_py; then python3 -m json.tool
  else cat
  fi
}

json_escape(){ # escape a value for embedding inside a JSON string
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

json_str(){ # json_str KEY  <<< json   -> scalar string ("" if absent)
  local k="$1"
  if have_jq; then
    jq -r --arg k "$k" '.[$k] // ""'
  elif have_py; then
    python3 -c 'import sys,json
try: d=json.load(sys.stdin)
except Exception: d={}
v=d.get(sys.argv[1],"")
print("" if v is None else v)' "$k"
  else
    sed -n "s/.*\"$k\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n 1
  fi
}

env_get(){ # env_get FILE KEY -> value ("" when absent/unreadable)
  local f="$1" k="$2"
  [ -r "$f" ] || return 0
  sed -n "s/^${k}=//p" "$f" | tail -n 1
}

env_set(){ # env_set FILE KEY VALUE (create-or-replace; escalates when needed)
  local f="$1" k="$2" v="$3" d
  d="$(dirname "$f")"
  if [ -f "$f" ] && [ -w "$f" ]; then
    if grep -q "^${k}=" "$f"; then sed -i "s|^${k}=.*|${k}=${v}|" "$f"
    else printf '%s=%s\n' "$k" "$v" >>"$f"; fi
    return 0
  fi
  if [ ! -e "$f" ] && [ -d "$d" ] && [ -w "$d" ]; then
    printf '%s=%s\n' "$k" "$v" >"$f"
    return 0
  fi
  if grep -q "^${k}=" "$f" 2>/dev/null; then
    as_root sed -i "s|^${k}=.*|${k}=${v}|" "$f"
  else
    as_root sh -c "printf '%s=%s\n' '$k' '$v' >>'$f'"
  fi
}

# ------------------------------------------------------------ URL normalizers
#   ipcod.example.com / https://host  ->  wss://host/ws      (agent)
#   ipcod.example.com / wss://host/ws ->  https://host/api/beacon (beacon)
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

normalize_beacon(){
  local u="${1%/}"
  case "$u" in
    http://*|https://*) : ;;
    ws://*)    u="https://${u#ws://}" ;;
    wss://*)   u="https://${u#wss://}" ;;
    *)         u="https://$u" ;;
  esac
  case "$u" in */api/beacon) : ;; *) u="$u/api/beacon" ;; esac
  printf '%s' "$u"
}

# ------------------------------------------------------- component resolution
comp_mode(){ # comp -> systemd | dev (beacon needs a systemd *user* session)
  if [ "${IPCOD_DEV:-0}" = 1 ]; then printf 'dev'; return 0; fi   # force plain processes
  if systemd_ok && { [ "$1" != beacon ] || user_systemd_ok; }; then
    printf 'systemd'
  else
    printf 'dev'
  fi
}

env_file(){ # comp -> config file used in the current mode
  case "$1" in
    coordinator) if [ "$(comp_mode "$1")" = systemd ]; then printf '%s' "$COORD_ENV"; else printf '%s' "$STATE_DIR/coordinator.env"; fi ;;
    agent)       if [ "$(comp_mode "$1")" = systemd ]; then printf '%s' "$AGENT_ENV";   else printf '%s' "$STATE_DIR/agent.env"; fi ;;
    beacon)      printf '%s' "$BEACON_ENV" ;;
    *) die "unknown component: $1" ;;
  esac
}

unit_name(){ case "$1" in coordinator) echo "$COORD_UNIT";; agent) echo "$AGENT_UNIT";; beacon) echo "$BEACON_UNIT";; esac; }

unit_scope(){ case "$1" in beacon) echo user;; *) echo system;; esac; }

unit_file(){ # comp -> unit file path
  case "$1" in
    beacon) printf '%s' "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/$BEACON_UNIT.service" ;;
    *)      printf '%s' "/etc/systemd/system/$(unit_name "$1").service" ;;
  esac
}

comp_bin(){ # comp -> best available binary (installed, else freshly built)
  local b
  case "$1" in
    coordinator) b="$COORD_BIN" ;;
    agent)       b="$AGENT_BIN" ;;
    beacon)      b="$BEACON_BIN" ;;
  esac
  [ -x "$b" ] || b="$DIST/ipcod-$1"
  printf '%s' "$b"
}

have_bin(){ [ -x "$(comp_bin "$1")" ]; }

# ------------------------------------------------------------- service control
sys_ctl(){ # scope verb [args...] — scope is system|user
  local scope="$1"; shift
  if [ "$scope" = user ]; then
    systemctl --user "$@"
  elif is_root; then
    systemctl "$@"
  else
    sudo systemctl "$@"
  fi
}

unit_installed(){ [ -f "$(unit_file "$1")" ]; }
unit_active(){ sys_ctl "$(unit_scope "$1")" is-active --quiet "$(unit_name "$1")" 2>/dev/null; }

dev_pidfile(){ printf '%s' "$STATE_DIR/$1.pid"; }
dev_pid(){ [ -f "$(dev_pidfile "$1")" ] && cat "$(dev_pidfile "$1")" 2>/dev/null || true; }
dev_alive(){ local p; p="$(dev_pid "$1")"; [ -n "$p" ] && kill -0 "$p" 2>/dev/null; }

dev_start(){ # comp — background process fallback for non-systemd hosts
  local c="$1" bin envf
  bin="$(comp_bin "$c")"; envf="$(env_file "$c")"
  [ -x "$bin" ] || die "no binary for $c (looked at $bin) — run: ./manage.sh build"
  [ -r "$envf" ] || die "no config for $c at $envf — run: ./manage.sh setup $c"
  dev_alive "$c" && { warn "$c is already running (pid $(dev_pid "$c"))"; return 0; }
  mkdir -p "$LOG_DIR" "$STATE_DIR"
  # Ignore SIGHUP and detach stdin so the process outlives this terminal.
  # shellcheck disable=SC1090
  ( trap '' HUP; set -a; . "$envf"; set +a; exec "$bin" run ) \
    </dev/null >>"$LOG_DIR/$c.log" 2>&1 &
  echo $! >"$(dev_pidfile "$c")"
  sleep 0.3
  dev_alive "$c" && good "$c started (pid $(dev_pid "$c"), log $LOG_DIR/$c.log)" \
                  || die "$c failed to start — see $LOG_DIR/$c.log"
}

dev_stop(){ # comp
  local c="$1" p
  p="$(dev_pid "$c")"
  if [ -z "$p" ] || ! kill -0 "$p" 2>/dev/null; then
    info "$c is not running"
    rm -f "$(dev_pidfile "$c")"
    return 0
  fi
  kill "$p" 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$p" 2>/dev/null || break; sleep 0.3; done
  kill -0 "$p" 2>/dev/null && kill -9 "$p" 2>/dev/null || true
  rm -f "$(dev_pidfile "$c")"
  good "$c stopped"
}

svc_start(){ # comp
  local c="$1"
  if [ "$(comp_mode "$c")" = systemd ]; then
    unit_installed "$c" || { info "$c isn't installed yet — running setup"; setup_comp "$c"; }
    have_bin "$c" || install_bin "$c"
    sys_ctl "$(unit_scope "$c")" enable --now "$(unit_name "$c")" >/dev/null 2>&1 || true
    sleep 0.5
    unit_active "$c" && good "$(unit_name "$c") is active" \
                      || die "$(unit_name "$c") isn't active — check: journalctl -u $(unit_name "$c") -e"
  else
    dev_start "$c"
  fi
}

svc_stop(){ # comp
  local c="$1"
  if [ "$(comp_mode "$c")" = systemd ] && unit_installed "$c"; then
    info "stopping $(unit_name "$c") (a connected agent can take ~30s to wind down)"
    sys_ctl "$(unit_scope "$c")" disable --now "$(unit_name "$c")" >/dev/null 2>&1 || true
    good "$(unit_name "$c") stopped"
  else
    dev_stop "$c"
  fi
}

svc_restart(){ # comp
  local c="$1"
  if [ "$(comp_mode "$c")" = systemd ] && unit_installed "$c"; then
    info "restarting $(unit_name "$c") (a connected agent can take ~30s to wind down)"
    sys_ctl "$(unit_scope "$c")" restart "$(unit_name "$c")" >/dev/null 2>&1 \
      || die "restart failed — check: journalctl -u $(unit_name "$c") -e"
    good "$(unit_name "$c") restarted"
  else
    dev_stop "$c"; dev_start "$c"
  fi
}

svc_logs(){ # comp [--follow]
  local c="$1" follow="${2:-}"
  if [ "$(comp_mode "$c")" = systemd ] && unit_installed "$c"; then
    local args=(-u "$(unit_name "$c")" -n 200)
    [ "$(unit_scope "$c")" = user ] && args=(--user "${args[@]}")
    if [ "$follow" = "-f" ] || [ "$follow" = "--follow" ]; then
      journalctl "${args[@]}" -f
    else
      journalctl "${args[@]}" --no-pager
    fi
  else
    local f="$LOG_DIR/$c.log"
    [ -f "$f" ] || die "no log at $f"
    if [ "$follow" = "-f" ] || [ "$follow" = "--follow" ]; then tail -n 200 -f "$f"; else tail -n 200 "$f"; fi
  fi
}

# ------------------------------------------------------------------- binaries
build_binaries(){
  info "building binaries (make build)"
  if command_ok make && command_ok go; then
    ( cd "$REPO_DIR" && make build ) >/dev/null || die "build failed"
  elif command_ok go; then
    mkdir -p "$DIST"
    local t name pkg
    for t in ipcod-coordinator:coordinator ipcod-agent:agent ipcod-beacon:beacon ipcod:ipcod; do
      name="${t%%:*}"; pkg="${t##*:}"
      ( cd "$REPO_DIR" && CGO_ENABLED=0 go build -trimpath -o "$DIST/$name" "./cmd/$pkg" ) || die "build failed for $name"
    done
  else
    die "need 'make' + 'go' to build — install Go, or drop release binaries into $DIST/"
  fi
  good "binaries ready in $DIST"
}

ensure_bins(){ # build if the dist binaries are missing
  [ -x "$DIST/ipcod-coordinator" ] && [ -x "$DIST/ipcod-agent" ] && [ -x "$DIST/ipcod-beacon" ] && return 0
  build_binaries
}

install_bin(){ # comp — copy the binary to its system/user location
  local c="$1" src dst
  case "$c" in
    beacon) dst="$BEACON_BIN" ;;
    *)      dst="/usr/local/bin/ipcod-$c" ;;
  esac
  # Prefer a freshly built dist binary; fall back to what's already installed.
  if [ -x "$DIST/ipcod-$c" ]; then
    src="$DIST/ipcod-$c"
  elif [ -x "$dst" ]; then
    return 0 # already in place and no newer build to copy
  else
    ensure_bins; src="$DIST/ipcod-$c"
  fi
  [ "$src" = "$dst" ] && return 0
  case "$c" in
    beacon) mkdir -p "$(dirname "$dst")"; install -m 0755 "$src" "$dst" ;;
    *)      as_root install -d -m 0755 /usr/local/bin; as_root install -m 0755 "$src" "$dst" ;;
  esac
  good "installed $dst"
}

# ------------------------------------------------------------------- admin API
url_base(){ # wss://host/ws | https://host/api/beacon | host:port -> base URL
  local u="${1%/}"
  case "$u" in
    ws://*)  u="http://${u#ws://}" ;;
    wss://*) u="https://${u#wss://}" ;;
    http://*|https://*) : ;;
    *) u="http://$u" ;;
  esac
  u="${u%/ws}"; u="${u%/api/beacon}"
  printf '%s' "${u%/}"
}

admin_base(){ # where to reach the coordinator's API from here
  if [ -n "${IPCOD_URL:-}" ]; then printf '%s' "${IPCOD_URL%/}"; return 0; fi
  local listen hostport
  listen="$(env_get "$(env_file coordinator)" IPCOD_LISTEN)"
  if [ -n "$listen" ]; then
    # local coordinator: talk to its loopback bind address
    hostport="${listen#\[}"; hostport="${hostport%%]*}"; hostport="${hostport##*:}"
    [ -n "$hostport" ] || hostport=8443
    printf 'http://127.0.0.1:%s' "$hostport"
    return 0
  fi
  # no local coordinator config — reuse the agent/beacon-style URL if given
  url_base "${IPCOD_COORDINATOR:-http://127.0.0.1:8443}"
}

api_ready(){ # nicest-possible reason when the admin API can't be used yet
  command_ok curl || { warn "curl is required for admin API calls"; return 1; }
  if [ -z "$(admin_token)" ]; then
    if [ -r "$(env_file coordinator)" ]; then
      warn "no admin token in $(env_file coordinator) (try sudo, or set IPCOD_ADMIN_TOKEN)"
    else
      warn "no coordinator configured yet — run: ./manage.sh setup coordinator"
    fi
    return 1
  fi
  return 0
}

admin_token(){ # from the local coordinator env, or IPCOD_ADMIN_TOKEN
  local t
  t="$(env_get "$(env_file coordinator)" IPCOD_ADMIN_TOKEN)"
  [ -n "$t" ] || t="${IPCOD_ADMIN_TOKEN:-}"
  printf '%s' "$t"
}

admin_req(){ # admin_req METHOD PATH [JSON-BODY] -> body on stdout, dies on error
  local method="$1" path="$2" data="${3:-}" tok out code body url
  command_ok curl || die "curl is required for admin API calls"
  tok="$(admin_token)"
  [ -n "$tok" ] || die "no admin token found in $(env_file coordinator) — re-run setup, or read it with sudo"
  url="$(admin_base)$path"
  if [ -n "$data" ]; then
    out="$(curl -sS -m 15 -w $'\n%{http_code}' -X "$method" \
      -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
      -d "$data" "$url" 2>&1)" || die "request to $url failed: $out"
  else
    out="$(curl -sS -m 15 -w $'\n%{http_code}' -X "$method" \
      -H "Authorization: Bearer $tok" "$url" 2>&1)" || die "request to $url failed: $out"
  fi
  code="${out##*$'\n'}"; body="${out%$'\n'*}"
  case "$code" in
    2??) printf '%s' "$body" ;;
    401) die "admin API unauthorized — wrong ADMIN_TOKEN in $(env_file coordinator)?" ;;
    *)   die "admin API $method $path -> HTTP $code: $body" ;;
  esac
}

show_token_once(){ # pretty-print a freshly minted token
  printf '\n  %sToken (copy it now — it is shown ONCE):%s\n' "$Y" "$R"
  printf '  %s%s%s\n\n' "$B" "$1" "$R"
}

# ----------------------------------------------------------------------- setup
setup_coordinator(){
  ensure_bins
  if [ "$(comp_mode coordinator)" = systemd ]; then
    local was_active=0
    unit_installed coordinator && unit_active coordinator && was_active=1
    info "launching the coordinator installer (systemd)"
    local listen cur_token tp
    listen="${LISTEN:-$(env_get "$COORD_ENV" IPCOD_LISTEN)}"
    cur_token="${ADMIN_TOKEN:-$(env_get "$COORD_ENV" IPCOD_ADMIN_TOKEN)}"
    tp="${TRUST_PROXY:-}"
    [ -n "$tp" ] || { tp="$(env_get "$COORD_ENV" IPCOD_TRUST_PROXY)"; }
    tp="$(yn "$tp")"
    as_root env LISTEN="$listen" ADMIN_TOKEN="$cur_token" TRUST_PROXY="$tp" \
      "$DEPLOY/install-coordinator.sh"
    if [ "$was_active" = 1 ]; then
      sys_ctl system restart "$COORD_UNIT" >/dev/null 2>&1 || true
      good "coordinator restarted with the new config"
    fi
  else
    info "no systemd here — doing a local (dev) coordinator setup"
    local listen token tp confirm_gen=n
    prompt "Listen address" "${LISTEN:-$(env_get "$(env_file coordinator)" IPCOD_LISTEN)}"
    listen="$REPLY"
    [ -n "$listen" ] || listen=":8443"
    token="${ADMIN_TOKEN:-$(env_get "$(env_file coordinator)" IPCOD_ADMIN_TOKEN)}"
    if [ -z "$token" ]; then
      confirm "Generate an admin bearer token?" y; [ "$REPLY" = y ] && confirm_gen=y
      if [ "$confirm_gen" = y ]; then
        token="$(command_ok openssl && openssl rand -base64 32 || head -c 32 /dev/urandom | base64)"
        show_token_once "$token"
      else
        prompt "Paste your admin token" ""; token="$REPLY"
      fi
    else
      info "keeping the provided admin token"
    fi
    [ -n "$token" ] || die "admin token is required"
    confirm "Behind a reverse proxy / tunnel (trust X-Forwarded-For)?" \
      "$([ "${TRUST_PROXY:-0}" = 1 ] && echo y || echo n)"
    [ "$REPLY" = y ] && tp=1 || tp=0
    mkdir -p "$STATE_DIR"
    local f="$(env_file coordinator)"
    cat >"$f" <<EOF
# ipcod coordinator config (dev mode, written by manage.sh)
IPCOD_LISTEN=$listen
IPCOD_ADMIN_TOKEN=$token
IPCOD_TRUST_PROXY=$tp
EOF
    chmod 0600 "$f"
    good "wrote $f"
    install_bin coordinator
    dev_start coordinator
  fi
  printf '\n  Next: open the dashboard, sign in with the admin token, and create\n'
  printf '  an agent + a beacon — or use %s./manage.sh create-agent <name> <port>%s.\n\n' "$DIM" "$R"
}

setup_agent(){
  ensure_bins
  if [ "$(comp_mode agent)" = systemd ]; then
    local was_active=0
    unit_installed agent && unit_active agent && was_active=1
    info "launching the agent installer (systemd)"
    local coord token name dr
    coord="${COORD:-$(env_get "$AGENT_ENV" IPCOD_COORDINATOR)}"
    token="${TOKEN:-$(env_get "$AGENT_ENV" IPCOD_TOKEN)}"
    name="${NAME:-$(env_get "$AGENT_ENV" IPCOD_NAME)}"
    # DRY_RUN passes through as 1/0 (or empty) — install-agent.sh treats
    # anything but "1" as "no", so never translate it to y/n here.
    dr="${DRY_RUN:-$(env_get "$AGENT_ENV" IPCOD_DRY_RUN)}"
    case "$dr" in ""|0|1) : ;; *) die "DRY_RUN must be 1 or 0" ;; esac
    as_root env COORD="$coord" TOKEN="$token" NAME="$name" DRY_RUN="$dr" \
      "$DEPLOY/install-agent.sh"
    if [ "$was_active" = 1 ]; then
      sys_ctl system restart "$AGENT_UNIT" >/dev/null 2>&1 || true
      good "agent restarted with the new config"
    fi
  else
    info "no systemd here — doing a local (dev) agent setup"
    local coord token name dr
    prompt "Coordinator WebSocket URL (host is fine)" \
      "${COORD:-$(env_get "$(env_file agent)" IPCOD_COORDINATOR)}"
    coord="$(normalize_ws "$REPLY")"
    [ "$coord" != "ws:///ws" ] || die "coordinator URL is required"
    prompt "Agent token (${B}ipca_…${R}, from the dashboard)" \
      "${TOKEN:-$(env_get "$(env_file agent)" IPCOD_TOKEN)}"
    token="$REPLY"
    [ -n "$token" ] || die "agent token is required — create an agent in the dashboard first"
    case "$token" in
      ipcb_*) die "that is an ipcb_ BEACON token — the agent needs an ipca_ token (create one in the dashboard)" ;;
    esac
    prompt "Display name" "${NAME:-$(env_get "$(env_file agent)" IPCOD_NAME)}"
    name="$REPLY"
    [ -n "$name" ] || name="$(hostname 2>/dev/null || echo ipcod)"
    prompt "Dry-run? 1 = log ufw commands only, 0 = manage the firewall" \
      "${DRY_RUN:-$(env_get "$(env_file agent)" IPCOD_DRY_RUN)}"
    dr="$REPLY"
    [ -n "$dr" ] || dr=1
    case "$dr" in 0|1) : ;; *) die "dry-run must be 1 (log only) or 0 (manage ufw)" ;; esac
    mkdir -p "$STATE_DIR"
    local f="$(env_file agent)"
    cat >"$f" <<EOF
# ipcod agent config (dev mode, written by manage.sh)
IPCOD_COORDINATOR=$coord
IPCOD_TOKEN=$token
IPCOD_NAME=$name
IPCOD_UFW=ufw
IPCOD_DRY_RUN=$dr
EOF
    chmod 0600 "$f"
    good "wrote $f"
    install_bin agent
    dev_start agent
  fi
}

setup_beacon(){
  ensure_bins
  local coord token interval
  prompt "Coordinator beacon endpoint (host is fine)" \
    "${COORD:-$(env_get "$BEACON_ENV" IPCOD_COORDINATOR)}"
  coord="$(normalize_beacon "$REPLY")"
  [ "$coord" != "https:///api/beacon" ] || die "coordinator endpoint is required"
  prompt "Beacon token (${B}ipcb_…${R}, from the dashboard)" \
    "${TOKEN:-$(env_get "$BEACON_ENV" IPCOD_TOKEN)}"
  token="$REPLY"
  [ -n "$token" ] || die "beacon token is required — create a beacon in the dashboard first"
  case "$token" in
    ipca_*) die "that is an ipca_ AGENT token — the beacon needs an ipcb_ token (create one in the dashboard)" ;;
  esac
  prompt "Check-in interval" "${INTERVAL:-$(env_get "$BEACON_ENV" IPCOD_BEACON_INTERVAL)}"
  interval="$REPLY"
  [ -n "$interval" ] || interval=30s

  if is_root; then
    warn "the beacon normally runs as YOUR login user, not root — this installs it for root"
  fi

  mkdir -p "$(dirname "$BEACON_ENV")"
  cat >"$BEACON_ENV" <<EOF
# ipcod beacon config (written by manage.sh)
IPCOD_COORDINATOR=$coord
IPCOD_TOKEN=$token
# Read by manage.sh only; the interval is passed on the ExecStart line.
IPCOD_BEACON_INTERVAL=$interval
EOF
  chmod 0600 "$BEACON_ENV"
  good "wrote $BEACON_ENV"
  install_bin beacon

  if [ "$(comp_mode beacon)" = systemd ]; then
    mkdir -p "$(dirname "$(unit_file beacon)")"
    local exec_start="$BEACON_BIN run"
    [ "$interval" != 30s ] && exec_start="$exec_start -interval $interval"
    cat >"$(unit_file beacon)" <<EOF
[Unit]
Description=ipcod beacon (reports laptop public IP to coordinator)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$BEACON_ENV
ExecStart=$exec_start
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
    systemctl --user daemon-reload
    if unit_active beacon; then
      systemctl --user restart "$BEACON_UNIT" >/dev/null 2>&1 || true   # pick up new env/token
    else
      systemctl --user enable --now "$BEACON_UNIT" >/dev/null 2>&1 || true
    fi
    sleep 0.5
    unit_active beacon && good "$BEACON_UNIT (user unit) is active" \
                        || warn "beacon isn't active yet — check: journalctl --user -u $BEACON_UNIT -e"
    confirm "Enable lingering so the beacon runs while logged out?" y
    if [ "$REPLY" = y ]; then
      if command_ok loginctl; then
        loginctl enable-linger "${SUDO_USER:-$USER}" 2>/dev/null \
          && good "lingering enabled for ${SUDO_USER:-$USER}" \
          || warn "could not enable linger (needs root or lingering already managed)"
      else
        warn "loginctl not found — skipping linger"
      fi
    fi
  else
    dev_start beacon
  fi
  printf '\n  Verify with: %s./manage.sh health%s\n\n' "$DIM" "$R"
}

setup_comp(){ case "$1" in coordinator) setup_coordinator;; agent) setup_agent;; beacon) setup_beacon;; esac; }

# --------------------------------------------------------------------- health
health_coordinator(){
  local url code hostport listen
  listen="$(env_get "$(env_file coordinator)" IPCOD_LISTEN)"
  if [ -n "$listen" ]; then
    hostport="${listen#\[}"; hostport="${hostport%%]*}"; hostport="${hostport##*:}"
    url="http://127.0.0.1:${hostport:-8443}/healthz"
  else
    url="$(url_base "${IPCOD_COORDINATOR:-http://127.0.0.1:8443}")/healthz"
  fi
  command_ok curl || { warn "curl missing — cannot probe $url"; return 0; }
  code="$(curl -s -o /dev/null -m 3 -w '%{http_code}' "$url" 2>/dev/null || true)"
  case "$code" in
    200) good "coordinator healthz OK ($url)" ;;
    000) warn "coordinator not answering at $url" ;;
    *)   warn "coordinator healthz returned HTTP $code" ;;
  esac
}

health_agent(){
  local bin f coord token
  bin="$(comp_bin agent)"
  [ -x "$bin" ] || { warn "agent binary not found"; return 0; }
  f="$(env_file agent)"
  coord="$(env_get "$f" IPCOD_COORDINATOR)"; token="$(env_get "$f" IPCOD_TOKEN)"
  if [ -z "$coord" ] || [ -z "$token" ]; then
    warn "agent not configured (no coordinator/token in $f)"
    return 0
  fi
  if "$bin" check -coordinator "$coord" -token "$token" >/dev/null 2>&1; then
    good "agent check OK (auth + connectivity)"
  else
    warn "agent check failed — run: ./manage.sh logs agent"
  fi
}

health_beacon(){
  local bin f coord token
  bin="$(comp_bin beacon)"
  [ -x "$bin" ] || { warn "beacon binary not found"; return 0; }
  f="$(env_file beacon)"
  coord="$(env_get "$f" IPCOD_COORDINATOR)"; token="$(env_get "$f" IPCOD_TOKEN)"
  if [ -z "$coord" ] || [ -z "$token" ]; then
    warn "beacon not configured (no coordinator/token in $f)"
    return 0
  fi
  local out
  if out="$("$bin" once -coordinator "$coord" -token "$token" 2>&1)"; then
    good "beacon once OK — $out"
  else
    warn "beacon once failed — $out"
  fi
}

cmd_health(){
  banner
  printf '%s── Health checks%s\n' "$B" "$R"
  health_coordinator
  health_agent
  health_beacon
  if [ -r "$(env_file coordinator)" ] && [ -n "$(admin_token)" ] && command_ok curl; then
    admin_req GET /api/admin/status | json_pp || true
  fi
  printf '\n'
}

# --------------------------------------------------------------- status view
comp_state(){ # comp -> "active" | "stopped" | "pid N" | "not installed"
  if [ "$(comp_mode "$1")" = systemd ]; then
    if ! unit_installed "$1"; then echo "not installed"; return; fi
    if unit_active "$1"; then echo "active"; else echo "stopped"; fi
  else
    if dev_alive "$1"; then echo "pid $(dev_pid "$1")"; else echo "stopped"; fi
  fi
}

cmd_status(){
  banner
  local mode
  mode="$(comp_mode coordinator)"
  if [ "$mode" = dev ]; then
    printf '%s── Services%s   %s(dev mode — state in %s)%s\n' "$B" "$R" "$DIM" "$STATE_DIR" "$R"
  else
    printf '%s── Services%s\n' "$B" "$R"
  fi
  local c
  for c in $COMPONENTS; do
    printf '  %-12s %s\n' "$c" "$(comp_state "$c")"
  done

  if [ -n "$(admin_token)" ] && command_ok curl; then
    printf '\n%s── Coordinator%s\n' "$B" "$R"
    local st ip rev conn
    st="$(admin_req GET /api/admin/status 2>/dev/null || true)"
    if [ -n "$st" ]; then
      ip="$(printf '%s' "$st" | json_str ip)"
      rev="$(printf '%s' "$st" | json_str revision)"
      conn="$(printf '%s' "$st" | json_str connected_agents)"
      printf '  laptop IP   : %s%s%s  (revision %s)\n' "$G" "${ip:-—}" "$R" "${rev:-0}"
      printf '  agents live : %s\n' "${conn:-0}"
    else
      warn "could not reach the admin API at $(admin_base)"
    fi
    cmd_list quiet
  elif [ ! -r "$(env_file coordinator)" ]; then
    printf '\n  %snot set up yet — run: ./manage.sh setup coordinator%s\n' "$DIM" "$R"
  else
    printf '\n  %sadmin token not readable (try sudo) — skipping API summary%s\n' "$DIM" "$R"
  fi
  printf '\n'
}

# ------------------------------------------------------- agents & beacons CRUD
list_kind(){ # agent|beacon
  local kind="$1" path items
  case "$kind" in
    agent)  path=/api/admin/agents;  items=.agents  ;;
    beacon) path=/api/admin/beacons; items=.beacons ;;
    *) die "unknown kind: $kind" ;;
  esac
  local body
  body="$(admin_req GET "$path")"
  if have_jq; then
    printf '%s' "$body" | jq -r \
      "$items[] | \"  \(.id)  \(.name)  port=\(.port // \"-\")  enabled=\(.enabled)  last_ip=\(.last_ip // \"-\")  seen=\(.last_seen // \"-\")\""
  elif have_py; then
    printf '%s' "$body" | python3 -c '
import sys, json
d = json.load(sys.stdin)
items = d.get("agents") or d.get("beacons") or []
for it in items:
    extra = "port=%s " % it["port"] if "port" in it else ""
    print("  %s  %s  %senabled=%s  last_ip=%s  seen=%s" % (
        it.get("id",""), it.get("name",""), extra, it.get("enabled",""),
        it.get("last_ip") or "-", it.get("last_seen") or "-"))'
  else
    printf '%s' "$body" | json_pp
  fi
}

cmd_list(){ # [quiet]
  local quiet="${1:-}"
  if [ "$quiet" != quiet ]; then banner; printf '%s── Agents%s\n' "$B" "$R"; fi
  api_ready || return 0
  list_kind agent || true
  [ "$quiet" = quiet ] && return 0
  printf '\n%s── Beacons%s\n' "$B" "$R"
  list_kind beacon || true
  printf '\n'
}

find_id_by_name(){ # kind name -> id ("" if not found)
  local kind="$1" name="$2" path body
  case "$kind" in agent) path=/api/admin/agents;; beacon) path=/api/admin/beacons;; esac
  body="$(admin_req GET "$path")"
  if have_jq; then
    printf '%s' "$body" | jq -r --arg n "$name" \
      '(.agents // .beacons)[] | select(.name == $n) | .id' | head -n 1
  elif have_py; then
    printf '%s' "$body" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for it in (d.get("agents") or d.get("beacons") or []):
    if it.get("name") == sys.argv[1]:
        print(it.get("id","")); break' "$name"
  fi
}

resolve_id(){ # kind name-or-id -> id
  local kind="$1" ref="$2" id
  id="$(find_id_by_name "$kind" "$ref")"
  [ -n "$id" ] && printf '%s' "$id" || printf '%s' "$ref"
}

cmd_create_agent(){ # name port [proto]
  banner
  local name="${1:-}" port="${2:-}" proto="${3:-}"
  [ -n "$name" ] || { prompt "Agent name"; name="$REPLY"; }
  [ -n "$port" ] || { prompt "Port to guard (e.g. 22)"; port="$REPLY"; }
  [ -n "$proto" ] || { prompt "Protocol (tcp/udp)" "tcp"; proto="$REPLY"; }
  case "$port" in ''|*[!0-9]*) die "port must be a number (got '$port')" ;; esac
  [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || die "port must be between 1 and 65535"
  case "$proto" in tcp|udp) : ;; *) die "protocol must be tcp or udp (got '$proto')" ;; esac
  local out tok
  out="$(admin_req POST /api/admin/agents \
    "{\"name\":\"$(json_escape "$name")\",\"port\":$port,\"proto\":\"$proto\"}")"
  tok="$(printf '%s' "$out" | json_str token)"
  good "agent '$name' created (port $port/$proto)"
  show_token_once "$tok"
  printf '  Use it on the VPS:\n'
  printf '    %ssudo COORD=wss://<your-coordinator>/ws TOKEN=%s ./deploy/install-agent.sh%s\n\n' "$DIM" "$tok" "$R"
}

cmd_create_beacon(){ # [name]
  banner
  local name="${1:-}"
  [ -n "$name" ] || { prompt "Beacon name (e.g. laptop)"; name="$REPLY"; }
  local out tok
  out="$(admin_req POST /api/admin/beacons "{\"name\":\"$(json_escape "$name")\"}")"
  tok="$(printf '%s' "$out" | json_str token)"
  good "beacon '$name' created"
  show_token_once "$tok"
  printf '  Use it on your laptop:\n'
  printf '    %s./manage.sh setup beacon%s   (paste the token when asked)\n\n' "$DIM" "$R"
}

cmd_rotate(){ # agent|beacon name-or-id
  banner
  local kind="$1" ref="${2:-}" id out tok
  [ -n "$ref" ] || { list_kind "$kind"; prompt "Which $kind (name or id)"; ref="$REPLY"; }
  id="$(resolve_id "$kind" "$ref")"
  confirm "Rotate the token for $kind '$ref'? The old token stops working immediately." n
  [ "$REPLY" = y ] || { info "aborted"; return 0; }
  out="$(admin_req POST "/api/admin/${kind}s/$id/rotate")"
  tok="$(printf '%s' "$out" | json_str token)"
  good "token rotated"
  show_token_once "$tok"
  # Keep the local service config in sync when we own it here.
  local f
  f="$(env_file "$kind")"
  if [ -r "$f" ] && grep -q '^IPCOD_TOKEN=' "$f" 2>/dev/null; then
    confirm "Write the new token to $f and restart $kind?" y
    if [ "$REPLY" = y ]; then
      env_set "$f" IPCOD_TOKEN "$tok"
      good "updated $f"
      svc_restart "$kind"
      return 0
    fi
  fi
  printf '  Update the %s config with the new token, then restart it.\n\n' "$kind"
}

cmd_toggle(){ # agent|beacon name-or-id on|off
  local kind="$1" ref="$2" state="$3" id enabled
  banner
  [ -n "$ref" ] || { list_kind "$kind"; prompt "Which $kind (name or id)"; ref="$REPLY"; }
  [ "$state" = on ] && enabled=true || enabled=false
  id="$(resolve_id "$kind" "$ref")"
  admin_req PATCH "/api/admin/${kind}s/$id" "{\"enabled\":$enabled}" >/dev/null
  good "$kind '$ref' is now $state"
}

cmd_delete(){ # agent|beacon name-or-id
  local kind="$1" ref="${2:-}" id
  banner
  [ -n "$ref" ] || { list_kind "$kind"; prompt "Which $kind (name or id)"; ref="$REPLY"; }
  confirm "Delete $kind '$ref' and revoke its token? This cannot be undone." n
  [ "$REPLY" = y ] || { info "aborted"; return 0; }
  id="$(resolve_id "$kind" "$ref")"
  admin_req DELETE "/api/admin/${kind}s/$id" >/dev/null
  good "$kind '$ref' deleted"
}

# ------------------------------------------------------------------- IP tools
cmd_ip(){ # [new-ip]
  local cur new
  cur="$(admin_req GET /api/admin/status | json_str ip)"
  if [ -n "${1:-}" ]; then
    new="$1"
    case "$new" in
      *[!0-9a-fA-F:.]*) die "'$new' doesn't look like an IPv4/IPv6 address" ;;
    esac
    confirm "Force the laptop IP to $new (overrides beacon detection until it changes)?" n
    [ "$REPLY" = y ] || { info "aborted"; return 0; }
    admin_req POST /api/admin/ip "{\"ip\":\"$new\"}" >/dev/null
    good "IP forced to $new — pushed to every connected agent"
  else
    printf '  current laptop IP: %s%s%s\n' "$G" "${cur:-—}" "$R"
  fi
}

cmd_token(){ # admin
  banner
  case "${1:-}" in
    admin|"")
      local t
      t="$(command_ok openssl && openssl rand -base64 32 || head -c 32 /dev/urandom | base64)"
      show_token_once "$t"
      printf '  Set it on the coordinator with:\n'
      printf '    %ssudo %s\n' "$DIM" "sh -c 'sed -i \"s|^IPCOD_ADMIN_TOKEN=.*|IPCOD_ADMIN_TOKEN=$t|\" $COORD_ENV && systemctl restart $COORD_UNIT'"
      printf '  or re-run %s./manage.sh setup coordinator%s.\n\n' "$DIM" "$R"
      ;;
    *) die "usage: ./manage.sh token admin" ;;
  esac
}

# ----------------------------------------------------------------- config edit
cmd_config(){ # comp [show|edit]
  local c="$1" mode="${2:-edit}" f
  f="$(env_file "$c")"
  case "$mode" in
    show) [ -r "$f" ] || die "no config at $f"; cat "$f" ;;
    edit)
      [ -r "$f" ] || die "no config at $f — run: ./manage.sh setup $c"
      if [ -w "$f" ]; then "$EDITOR_BIN" "$f"; else as_root "$EDITOR_BIN" "$f"; fi
      confirm "Restart $c to apply the changes?" y
      if [ "$REPLY" = y ]; then svc_restart "$c"; fi
      ;;
    *) die "usage: ./manage.sh config <component> [show|edit]" ;;
  esac
}

# ----------------------------------------------------------------- dry-run on/off
cmd_dryrun(){ # on|off
  local f v
  f="$(env_file agent)"
  [ -r "$f" ] || die "agent not configured — run: ./manage.sh setup agent"
  case "${1:-}" in
    on)  v=1 ;;
    off) v=0 ;;
    "")
      while true; do
        prompt "Agent dry-run? (on = log ufw only, off = manage the firewall)" "on"
        case "$REPLY" in on) v=1; break ;; off) v=0; break ;; esac
        [ "$INTERACTIVE" = 1 ] || die "answer must be on or off"
      done
      ;;
    *) die "usage: ./manage.sh dry-run on|off" ;;
  esac
  env_set "$f" IPCOD_DRY_RUN "$v"
  good "IPCOD_DRY_RUN=$v in $f"
  if [ "$v" = 0 ]; then
    warn "the agent will now REALLY change ufw rules — keep a backup access path"
  fi
  svc_restart agent
}

# ------------------------------------------------------------------- uninstall
uninstall_comp(){
  local c="$1"
  banner
  confirm "Uninstall the $c (stop service, remove unit/binary/config)?" n
  [ "$REPLY" = y ] || { info "aborted"; return 0; }

  # Stop in whichever mode it runs; also sweep a leftover unit from the other
  # mode so a previous install never lingers behind.
  if [ "$(comp_mode "$c")" = systemd ] && unit_installed "$c"; then
    sys_ctl "$(unit_scope "$c")" disable --now "$(unit_name "$c")" >/dev/null 2>&1 || true
    good "stopped $(unit_name "$c")"
  else
    dev_stop "$c"
    unit_installed "$c" && sys_ctl "$(unit_scope "$c")" disable --now "$(unit_name "$c")" >/dev/null 2>&1 || true
  fi
  if unit_installed "$c"; then
    as_root rm -f "$(unit_file "$c")"
    if [ "$(unit_scope "$c")" = user ]; then
      systemctl --user daemon-reload 2>/dev/null || true
      systemctl --user reset-failed "$(unit_name "$c")" 2>/dev/null || true
    else
      as_root systemctl daemon-reload
      as_root systemctl reset-failed "$(unit_name "$c")" 2>/dev/null || true
    fi
    good "removed unit $(unit_name "$c")"
  fi
  local f="$(env_file "$c")"
  if [ -f "$f" ]; then
    if [ -w "$f" ]; then rm -f "$f"; else as_root rm -f "$f"; fi
    good "removed config $f"
  fi
  case "$c" in
    beacon) rm -f "$BEACON_BIN" ;;
    *)      as_root rm -f "/usr/local/bin/ipcod-$c" ;;
  esac
  good "removed binary"
  if [ "$c" = coordinator ]; then
    confirm "Also delete the database (agents, beacons, IP history)?" n
    if [ "$REPLY" = y ]; then
      # /var/lib/ipcod is a symlink into systemd's private dir when the unit
      # owns it; remove both so no stale DB survives a purge.
      as_root rm -rf /var/lib/ipcod /var/lib/private/ipcod
      good "removed the coordinator database (/var/lib/ipcod)"
    fi
  fi
  rm -f "$STATE_DIR/$c.pid"
}

# ---------------------------------------------------------------------- menu
menu_loop(){
  while true; do
    banner
    printf '  %scoordinator%s %s | %sagent%s %s | %sbeacon%s %s\n\n' \
      "$B" "$R" "$(comp_state coordinator)" \
      "$B" "$R" "$(comp_state agent)" \
      "$B" "$R" "$(comp_state beacon)"
    printf '   %s1)%s Status overview          %s8)%s Agents & beacons (list/create)\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s2)%s Setup / install          %s9)%s Rotate a token\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s3)%s Start                     %s10)%s Force laptop IP\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s4)%s Stop                      %s11)%s Build binaries\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s5)%s Restart                   %s12)%s Agent dry-run on/off\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s6)%s Logs                      %s13)%s Uninstall\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s7)%s Health checks             %s0)%s Quit\n\n' "$DIM" "$R" "$DIM" "$R"
    printf 'Choice: '
    local choice c
    IFS= read -r choice || choice=0
    # each action runs in a subshell so a hard failure returns to the menu
    case "$choice" in
      1)  ( cmd_status ) || true ;;
      2)  ( banner; pick_comp; setup_comp "$REPLY" ) || true ;;
      3)  ( banner; pick_comp; svc_start "$REPLY" ) || true ;;
      4)  ( banner; pick_comp; svc_stop "$REPLY" ) || true ;;
      5)  ( banner; pick_comp; svc_restart "$REPLY" ) || true ;;
      6)  ( banner; pick_comp; svc_logs "$REPLY" -f ) || true ;;
      7)  ( cmd_health ) || true ;;
      8)  ( agents_menu ) || true ;;
      9)  ( banner; printf 'Rotate token for:\n'; pick_comp; cmd_rotate "$REPLY" ) || true ;;
      10) ( banner; prompt "New laptop IP (empty to just show current)" ""; cmd_ip "$REPLY" ) || true ;;
      11) ( build_binaries ) || true ;;
      12) ( banner; cmd_dryrun ) || true ;;
      13) ( banner; pick_comp; uninstall_comp "$REPLY" ) || true ;;
      0|q|Q|"") return 0 ;;
      *) printf '%s\n' "  unknown choice" ;;
    esac
    [ "$choice" = 0 ] && return 0
    printf '\n%spress enter to continue…%s' "$DIM" "$R"
    IFS= read -r _ || true
  done
}

agents_menu(){
  while true; do
    banner
    api_ready || return 0
    printf '  %sAgents%s\n' "$B" "$R"; list_kind agent || true
    printf '\n  %sBeacons%s\n' "$B" "$R"; list_kind beacon || true
    printf '\n   %s1)%s Create agent      %s5)%s Disable agent/beacon\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s2)%s Create beacon     %s6)%s Enable agent/beacon\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s3)%s Rotate token      %s7)%s Delete agent/beacon\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s4)%s Refresh           %s0)%s Back\n\n' "$DIM" "$R" "$DIM" "$R"
    printf 'Choice: '
    local choice kind ref
    IFS= read -r choice || choice=0
    case "$choice" in
      1) ( cmd_create_agent ) || true ;;
      2) ( cmd_create_beacon ) || true ;;
      3) ( banner; printf 'Which kind?\n'; pick_comp; cmd_rotate "$REPLY" ) || true ;;
      4) : ;;
      5|6)
        ( banner; printf 'Which kind?\n'; pick_comp; kind="$REPLY"
          prompt "Name or id of the $kind" ""; ref="$REPLY"
          if [ "$choice" = 5 ]; then cmd_toggle "$kind" "$ref" off
          else cmd_toggle "$kind" "$ref" on; fi ) || true ;;
      7) ( banner; printf 'Which kind?\n'; pick_comp; cmd_delete "$REPLY" ) || true ;;
      0|q|Q|"") return 0 ;;
      *) printf '%s\n' "  unknown choice" ;;
    esac
    printf '\n%spress enter to continue…%s' "$DIM" "$R"
    IFS= read -r _ || true
  done
}

# ---------------------------------------------------------------------- help
usage(){
  cat <<EOF
${B}ipcod manage.sh${R} — set up and operate the coordinator, agent and beacon.

${B}Usage${R}
  ./manage.sh                     interactive menu
  ./manage.sh [-y] <command> [args]    scriptable mode (-y pre-answers confirms with yes)

${B}Env${R}
  IPCOD_URL=http://host:port       base URL for admin API calls (default: local coordinator)
  IPCOD_ADMIN_TOKEN=token          admin token when the local env file isn't readable
  IPCOD_COORDINATOR=url            derive the API base URL from a ws/beacon URL
  IPCOD_YES=1                      same as -y
  IPCOD_DEV=1                      force plain background processes (skip systemd)
  IPCOD_STATE_DIR=path             dev-mode state dir (default ~/.local/state/ipcod)
  COORD/TOKEN/NAME/DRY_RUN/INTERVAL   non-interactive setup defaults

${B}Commands${R}
  status                          services + coordinator summary
  setup      <comp>               install/configure a component
  start|stop|restart <comp>       control a component
  logs       <comp> [-f]          tail logs (journalctl or dev log)
  config     <comp> [show|edit]   view or edit a component's config
  health                          run health checks (healthz, agent check, beacon once)
  build                           build the binaries into dist/
  list                            list agents + beacons (admin API)
  create-agent <name> <port> [proto]   create an agent, print its token once
  create-beacon <name>            create a beacon, print its token once
  rotate      <comp> [name|id]    rotate a token
  enable|disable <comp> <name|id> toggle an agent/beacon
  delete      <comp> <name|id>    delete an agent/beacon
  ip          [addr]              show the laptop IP, or force-set it
  token       admin               generate a new admin token
  dry-run     on|off              toggle the agent's ufw dry-run mode
  uninstall   <comp>              remove a component (asks before purging the DB)
  version                         print binary versions
  help                            this text

${B}Components${R}
  coordinator | agent | beacon     (abbreviations c / a / b work too)

${B}Examples${R}
  ./manage.sh                                  # menu
  sudo ./manage.sh setup coordinator           # then setup agent / beacon
  ./manage.sh create-agent web-1 22
  ./manage.sh logs agent -f
  ./manage.sh dry-run off                      # go live with real ufw changes
EOF
}

cmd_version(){
  local b
  for b in ipcod-coordinator ipcod-agent ipcod-beacon; do
    if [ -x "$(comp_bin "${b#ipcod-}")" ]; then
      printf '  %-18s %s\n' "$b" "$("$(comp_bin "${b#ipcod-}")" version 2>/dev/null | head -n 1)"
    else
      printf '  %-18s %s(not installed)%s\n' "$b" "$DIM" "$R"
    fi
  done
}

# ------------------------------------------------------------------- dispatch
main(){
  while [ $# -gt 0 ]; do
    case "$1" in
      -y|--yes) YES=1; shift ;;
      *) break ;;
    esac
  done
  local cmd="${1:-menu}"
  [ $# -gt 0 ] && shift || true
  case "$cmd" in
    menu)                menu_loop ;;
    status)              cmd_status ;;
    setup)               pick_comp "${1:-}"; setup_comp "$REPLY" ;;
    start)               pick_comp "${1:-}"; svc_start "$REPLY" ;;
    stop)                pick_comp "${1:-}"; svc_stop "$REPLY" ;;
    restart)             pick_comp "${1:-}"; svc_restart "$REPLY" ;;
    logs)                pick_comp "${1:-}"; svc_logs "$REPLY" "${2:-}" ;;
    config)              pick_comp "${1:-}"; cmd_config "$REPLY" "${2:-edit}" ;;
    health)              cmd_health ;;
    build)               build_binaries ;;
    list|ls)             cmd_list ;;
    create-agent)        cmd_create_agent "${1:-}" "${2:-}" "${3:-}" ;;
    create-beacon)       cmd_create_beacon "${1:-}" ;;
    rotate)              pick_comp "${1:-}"; cmd_rotate "$REPLY" "${2:-}" ;;
    enable)              pick_comp "${1:-}"; cmd_toggle "$REPLY" "${2:-}" on ;;
    disable)             pick_comp "${1:-}"; cmd_toggle "$REPLY" "${2:-}" off ;;
    delete)              pick_comp "${1:-}"; cmd_delete "$REPLY" "${2:-}" ;;
    ip)                  cmd_ip "${1:-}" ;;
    token)               cmd_token "${1:-}" ;;
    dry-run|dryrun)      cmd_dryrun "${1:-}" ;;
    uninstall)           pick_comp "${1:-}"; uninstall_comp "$REPLY" ;;
    version|--version|-v) cmd_version ;;
    help|--help|-h)      usage ;;
    *)                   usage; die "unknown command: $cmd" ;;
  esac
}

main "$@"
