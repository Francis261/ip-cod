#!/usr/bin/env bash
#
# manage.sh — interactive lifecycle manager for ipcod.
#
# One script to set up and operate both roles:
#
#   coordinator  the control plane (systemd: ipcod-coordinator). Run it on
#                your laptop: it detects your public IP itself, pushes
#                changes to agents, and serves the web dashboard on demand
#                (`./manage.sh web 8443`).
#   agent        the per-VPS firewall daemon (systemd: ipcod-agent)
#
# Interactive (menu-driven) by default:
#
#   ./manage.sh
#
# Scriptable: every menu action is also a subcommand:
#
#   ./manage.sh status
#   sudo ./manage.sh setup coordinator
#   ./manage.sh restart agent
#   ./manage.sh web 8443
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

COMPONENTS="coordinator agent"

COORD_UNIT=ipcod-coordinator
AGENT_UNIT=ipcod-agent

COORD_ENV=/etc/ipcod/coordinator.env
AGENT_ENV=/etc/ipcod/agent.env

COORD_BIN=/usr/local/bin/ipcod-coordinator
AGENT_BIN=/usr/local/bin/ipcod-agent

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
  printf '%s\n'   "${B} ipcod manage — coordinator/agent${R}"
  printf '%s\n\n' "${B}════════════════════════════════════════${R}"
}

# Interactive only when stdin is a terminal; otherwise use env/defaults.
INTERACTIVE=0; [ -t 0 ] && INTERACTIVE=1
YES=0; case "${IPCOD_YES:-}" in 1|y|Y|yes) YES=1 ;; esac   # -y / --yes: pre-answer confirms with y

prompt(){   # prompt "message" "default" -> $REPLY (loops until non-empty)
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

confirm(){  # confirm "message" y|n -> $REPLY (honours -y/--yes)
  local msg="$1" def="${2:-n}" hint="" reply=""
  [ "$YES" = 1 ] && def=y
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

pick_comp(){ # [coordinator/agent] -> $REPLY (prompts when omitted)
  local r="${1:-}"
  case "$r" in
    co*|c|1) REPLY=coordinator; return 0 ;;
    a*|2)    REPLY=agent;       return 0 ;;
    b*|beacon|3) die "the beacon is gone — the coordinator now detects your public IP itself (see ./manage.sh status)" ;;
  esac
  if [ "$INTERACTIVE" = 1 ]; then
    printf '  %s1)%s coordinator   %s2)%s agent\n' "$DIM" "$R" "$DIM" "$R"
  fi
  while true; do
    [ "$INTERACTIVE" = 1 ] && printf 'Component: '
    if ! IFS= read -r r; then printf '\n'; die "aborted — end of input"; fi
    case "$r" in
      co*|c|1) REPLY=coordinator; return 0 ;;
      a*|2)    REPLY=agent;       return 0 ;;
      b*|beacon|3) [ "$INTERACTIVE" = 1 ] && printf '%s\n' "  the beacon was folded into the coordinator — answer coordinator or agent" ;;
      *) [ "$INTERACTIVE" = 1 ] && printf '%s\n' "  answer coordinator or agent" ;;
    esac
    [ "$INTERACTIVE" = 1 ] || die "component must be coordinator or agent"
  done
}

# ---------------------------------------------------------------- tiny tools
command_ok(){ command -v "$1" >/dev/null 2>&1; }
have_jq(){ command_ok jq; }
have_py(){ command_ok python3; }

yn(){ case "${1:-}" in 1|y|Y|yes|true) printf 'y' ;; *) printf 'n' ;; esac; }

systemd_ok(){ command_ok systemctl && [ -d /run/systemd/system ]; }

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

# ------------------------------------------------------- component resolution
comp_mode(){ # comp -> systemd | dev
  if [ "${IPCOD_DEV:-0}" = 1 ]; then printf 'dev'; return 0; fi   # force plain processes
  if systemd_ok; then printf 'systemd'; else printf 'dev'; fi
}

env_file(){ # comp -> config file used in the current mode
  case "$1" in
    coordinator) if [ "$(comp_mode "$1")" = systemd ]; then printf '%s' "$COORD_ENV"; else printf '%s' "$STATE_DIR/coordinator.env"; fi ;;
    agent)       if [ "$(comp_mode "$1")" = systemd ]; then printf '%s' "$AGENT_ENV";   else printf '%s' "$STATE_DIR/agent.env"; fi ;;
    *) die "unknown component: $1" ;;
  esac
}

unit_name(){ case "$1" in coordinator) echo "$COORD_UNIT";; agent) echo "$AGENT_UNIT";; *) die "unknown component: $1" ;; esac; }

unit_file(){ printf '%s' "/etc/systemd/system/$(unit_name "$1").service"; }

comp_bin(){ # comp -> best available binary (installed, else freshly built)
  local b
  case "$1" in
    coordinator) b="$COORD_BIN" ;;
    agent)       b="$AGENT_BIN" ;;
    *) die "unknown component: $1" ;;
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
unit_active(){ sys_ctl system is-active --quiet "$(unit_name "$1")" 2>/dev/null; }

dev_pidfile(){ printf '%s' "$STATE_DIR/$1.pid"; }
dev_pid(){ [ -f "$(dev_pidfile "$1")" ] && cat "$(dev_pidfile "$1")" 2>/dev/null || true; }
dev_alive(){ local p; p="$(dev_pid "$1")"; [ -n "$p" ] && kill -0 "$p" 2>/dev/null; }

dev_start(){ # comp — background process fallback for non-systemd hosts
  local c="$1" bin envf sub=run pargs=() p
  bin="$(comp_bin "$c")"; envf="$(env_file "$c")"
  [ -x "$bin" ] || die "no binary for $c (looked at $bin) — run: ./manage.sh build"
  [ -r "$envf" ] || die "no config for $c at $envf — run: ./manage.sh setup $c"
  dev_alive "$c" && { warn "$c is already running (pid $(dev_pid "$c"))"; return 0; }
  # Dashboard configured → start as `web -p <port>` instead of headless `run`.
  p="$(env_get "$envf" IPCOD_WEB_PORT)"
  if [ -n "$p" ] && [ "$p" != 0 ]; then sub=web; pargs=(-p "$p"); fi
  mkdir -p "$LOG_DIR" "$STATE_DIR"
  # Ignore SIGHUP and detach stdin so the process outlives this terminal.
  # shellcheck disable=SC1090
  ( trap '' HUP; set -a; . "$envf"; set +a; exec "$bin" "$sub" "${pargs[@]}" ) \
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
    sys_ctl system enable --now "$(unit_name "$c")" >/dev/null 2>&1 || true
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
    info "stopping $(unit_name "$c") (a cleanly stopped agent drops its ipcod rule; it re-applies on next start)"
    sys_ctl system disable --now "$(unit_name "$c")" >/dev/null 2>&1 || true
    good "$(unit_name "$c") stopped"
  else
    dev_stop "$c"
  fi
}

svc_restart(){ # comp
  local c="$1"
  if [ "$(comp_mode "$c")" = systemd ] && unit_installed "$c"; then
    info "restarting $(unit_name "$c") (connected agents reconnect automatically)"
    sys_ctl system restart "$(unit_name "$c")" >/dev/null 2>&1 \
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
    for t in ipcod-coordinator:coordinator ipcod-agent:agent ipcod:ipcod; do
      name="${t%%:*}"; pkg="${t##*:}"
      ( cd "$REPO_DIR" && CGO_ENABLED=0 go build -trimpath -o "$DIST/$name" "./cmd/$pkg" ) || die "build failed for $name"
    done
  else
    die "need 'make' + 'go' to build — install Go, or drop release binaries into $DIST/"
  fi
  good "binaries ready in $DIST"
}

ensure_bins(){ # build if the dist binaries are missing
  [ -x "$DIST/ipcod-coordinator" ] && [ -x "$DIST/ipcod-agent" ] && return 0
  build_binaries
}

install_bin(){ # comp — copy the binary to its system location
  local c="$1" src dst
  case "$c" in
    coordinator|agent) dst="/usr/local/bin/ipcod-$c" ;;
    *) die "unknown component: $c" ;;
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
  as_root install -d -m 0755 /usr/local/bin
  as_root install -m 0755 "$src" "$dst"
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
  # tolerate a pasted path (/ws, or the legacy /api/beacon endpoint)
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
  # no local coordinator config — reuse the agent-style URL if given
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

# ---------------------------------------------------------------- web dashboard
# The coordinator serves the UI only when run as `web` instead of `run`.
# systemd mode: a unit drop-in swaps ExecStart to `web -p <port>`.
# dev mode:     IPCOD_WEB_PORT in the dev config; dev_start launches `web -p`.

web_dropin(){ # comp -> systemd drop-in path ("" when the comp has no web mode)
  case "${1:-coordinator}" in
    coordinator) printf '%s' "/etc/systemd/system/$COORD_UNIT.service.d/web.conf" ;;
    *)           printf '' ;;
  esac
}

web_port(){ # comp -> dashboard port ("" when the dashboard is off)
  local c="${1:-coordinator}" f p
  if [ "$(comp_mode "$c")" = systemd ]; then
    f="$(web_dropin "$c")"
    { [ -n "$f" ] && [ -r "$f" ]; } || return 0
    p="$(sed -n 's/.*-p \([0-9]\{1,\}\).*/\1/p' "$f" | head -n 1)"
  else
    p="$(env_get "$(env_file "$c")" IPCOD_WEB_PORT)"
  fi
  printf '%s' "$p"
}

wait_listening(){ # port -> 0 once something HTTP answers on 127.0.0.1:port (~4s max)
  command_ok curl || return 0
  local port="$1" i=0 code
  while [ "$i" -lt 12 ]; do
    code="$(curl -s -o /dev/null -m 1 -w '%{http_code}' "http://127.0.0.1:$port/" 2>/dev/null || true)"
    case "$code" in ''|000) : ;; *) return 0 ;; esac
    sleep 0.3
    i=$((i+1))
  done
  return 1
}

cmd_web(){ # [port|off|on|status] — start/stop the coordinator's web dashboard
  banner
  local c=coordinator cur arg port f
  cur="$(web_port "$c")"
  arg="${1:-status}"
  [ "$arg" = on ] && arg="${cur:-8443}"

  case "$arg" in
    status|"" )
      if [ -z "$cur" ]; then
        info "web dashboard: OFF — the coordinator runs headless (agents + API)"
        info "start it with: ./manage.sh web 8443   (then open http://127.0.0.1:8443/)"
      else
        good "web dashboard: ON — http://127.0.0.1:$cur/  (use this host's address remotely)"
        if command_ok curl; then
          local code
          code="$(curl -s -o /dev/null -m 3 -w '%{http_code}' "http://127.0.0.1:$cur/" 2>/dev/null || true)"
          case "$code" in
            200) ;;
            000) warn "not answering at http://127.0.0.1:$cur/ — try: ./manage.sh restart coordinator" ;;
            *)   info "answering (HTTP $code) — a login form is expected without a token" ;;
          esac
        fi
      fi
      ;;
    off|0|no|n)
      if [ -z "$cur" ]; then info "web dashboard is already off"; return 0; fi
      confirm "Stop serving the web dashboard (the coordinator keeps running)?" y
      [ "$REPLY" = y ] || { info "aborted"; return 0; }
      if [ "$(comp_mode "$c")" = systemd ]; then
        f="$(web_dropin "$c")"
        if [ -e "$f" ]; then
          as_root rm -f "$f"
          as_root rmdir "$(dirname "$f")" 2>/dev/null || true
        fi
        as_root systemctl daemon-reload
      else
        env_set "$(env_file "$c")" IPCOD_WEB_PORT ""
      fi
      svc_restart "$c"
      good "dashboard off — coordinator is headless again"
      ;;
    *)
      port="$arg"
      case "$port" in ''|*[!0-9]*) die "port must be a number (got '$port')" ;; esac
      [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || die "port must be between 1 and 65535"
      if [ "$(comp_mode "$c")" = systemd ]; then
        unit_installed "$c" || { info "coordinator isn't installed yet — running setup"; setup_comp "$c"; }
        f="$(web_dropin "$c")"
        as_root mkdir -p "$(dirname "$f")"
        as_root tee "$f" >/dev/null <<EOF
# Written by manage.sh: run the coordinator as \`web -p $port\` (dashboard on).
[Service]
ExecStart=
ExecStart=$COORD_BIN web -p $port
EOF
        as_root systemctl daemon-reload
      else
        [ -r "$(env_file "$c")" ] || { info "coordinator isn't set up yet — running setup"; setup_comp "$c"; }
        ensure_bins
        env_set "$(env_file "$c")" IPCOD_WEB_PORT "$port"
      fi
      svc_restart "$c"
      # systemctl restart returns as soon as the process is forked; wait for
      # the actual bind so the success message (and the next status) is true.
      if wait_listening "$port"; then
        good "web dashboard on port $port — open http://127.0.0.1:$port/"
      else
        warn "started, but http://127.0.0.1:$port/ hasn't answered yet — check: ./manage.sh logs coordinator"
      fi
      ;;
  esac
}

web_menu(){ # interactive path behind menu option 14
  local cur
  cmd_web status
  printf '\n'
  cur="$(web_port coordinator)"
  if [ -n "$cur" ]; then
    prompt "New dashboard port (or 'off' to stop serving it)" "$cur"
  else
    prompt "Dashboard port to serve on (or 'off' to leave it off)" "8443"
  fi
  cmd_web "$REPLY"
}


# ----------------------------------------------------------------------- setup
setup_coordinator(){
  ensure_bins
  if [ "$(comp_mode coordinator)" = systemd ]; then
    info "launching the coordinator installer (systemd)"
    local listen cur_token tp wp
    listen="${LISTEN:-$(env_get "$COORD_ENV" IPCOD_LISTEN)}"
    cur_token="${ADMIN_TOKEN:-$(env_get "$COORD_ENV" IPCOD_ADMIN_TOKEN)}"
    tp="${TRUST_PROXY:-}"
    [ -n "$tp" ] || { tp="$(env_get "$COORD_ENV" IPCOD_TRUST_PROXY)"; }
    tp="$(yn "$tp")"
    # Web dashboard: WEB_PORT (env) forces the choice; otherwise leave WEB
    # unset so the installer asks interactively — or, when scripted, keeps
    # whatever is currently configured (passed through as WEB_PORT).
    wp="$(web_port coordinator)"
    local -a ienv=(LISTEN="$listen" ADMIN_TOKEN="$cur_token" TRUST_PROXY="$tp")
    if [ -n "${WEB_PORT:-}" ]; then
      case "$WEB_PORT" in
        off|0|no|n) ienv+=(WEB=n WEB_PORT=) ;;
        *)          ienv+=(WEB=y WEB_PORT="$WEB_PORT") ;;
      esac
    else
      ienv+=(WEB_PORT="$wp")
    fi
    as_root env "${ienv[@]}" "$DEPLOY/install-coordinator.sh"
  else
    info "no systemd here — doing a local (dev) coordinator setup"
    local listen token tp confirm_gen=n wp_cur wp=""
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
    # Web dashboard: on or off for this dev coordinator.
    wp_cur="$(web_port coordinator)"
    if [ -n "${WEB_PORT:-}" ]; then
      case "$WEB_PORT" in off|0|no|n) wp="" ;; *) wp="$WEB_PORT" ;; esac
    else
      confirm "Serve the web dashboard (admin UI in a browser)?" "$([ -n "$wp_cur" ] && echo y || echo n)"
      if [ "$REPLY" = y ]; then
        prompt "Dashboard port" "${wp_cur:-8443}"
        wp="$REPLY"
        case "$wp" in ''|*[!0-9]*) die "dashboard port must be a number (got '$wp')" ;; esac
      fi
    fi
    mkdir -p "$STATE_DIR"
    local f="$(env_file coordinator)"
    cat >"$f" <<EOF
# ipcod coordinator config (dev mode, written by manage.sh)
IPCOD_LISTEN=$listen
IPCOD_ADMIN_TOKEN=$token
IPCOD_TRUST_PROXY=$tp
# Dashboard port: empty = headless ("run" mode); set = web -p <port>.
IPCOD_WEB_PORT=$wp
# Keep the dev DB out of the repo working tree.
IPCOD_DB=$STATE_DIR/ipcod.db
EOF
    chmod 0600 "$f"
    good "wrote $f"
    install_bin coordinator
    dev_start coordinator
  fi
  printf '\n  Next: the dashboard is served on demand — %s./manage.sh web 8443%s —\n' "$DIM" "$R"
  printf '  sign in with the admin token, and create an agent with\n'
  printf '  %s./manage.sh create-agent <name> <port>%s.\n' "$DIM" "$R"
  printf '  The coordinator detects the public IP by itself; agents only need\n'
  printf '  %swss://<your-host>/ws%s.\n\n' "$DIM" "$R"
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
      ipcb_*) die "that's a legacy ipcb_ beacon token — agents need an ipca_ token (create one with ./manage.sh create-agent)" ;;
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

setup_comp(){ case "$1" in coordinator) setup_coordinator;; agent) setup_agent;; *) die "unknown component: $1" ;; esac; }

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

health_web(){ # probe the dashboard when it's enabled
  local p code
  p="$(web_port coordinator)"
  [ -n "$p" ] || return 0
  command_ok curl || { warn "curl missing — cannot probe the dashboard on :$p"; return 0; }
  code="$(curl -s -o /dev/null -m 3 -w '%{http_code}' "http://127.0.0.1:$p/" 2>/dev/null || true)"
  case "$code" in
    200) good "web dashboard OK (http://127.0.0.1:$p/)" ;;
    000) warn "web dashboard enabled but not answering at http://127.0.0.1:$p/" ;;
    *)   good "web dashboard answering on :$p (HTTP $code)" ;;
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

cmd_health(){
  banner
  printf '%s── Health checks%s\n' "$B" "$R"
  health_coordinator
  health_web
  health_agent
  if [ -r "$(env_file coordinator)" ] && [ -n "$(admin_token)" ] && command_ok curl; then
    local st ip
    st="$(admin_req GET /api/admin/status 2>/dev/null || true)"
    if [ -n "$st" ]; then
      printf '%s' "$st" | json_pp || true
      ip="$(printf '%s' "$st" | json_str ip)"
      if [ -n "$ip" ]; then
        good "public IP known: $ip"
      else
        warn "no public IP yet — detection is starting, or IPCOD_NO_DETECT=1 / -no-detect is set"
      fi
    fi
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
  local wp
  wp="$(web_port coordinator)"
  if [ -n "$wp" ]; then
    printf '  %-12s %s\n' "web ui" "http://127.0.0.1:$wp/"
  fi

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

# ---------------------------------------------------------------- agents CRUD
list_agents(){
  local body
  body="$(admin_req GET /api/admin/agents)"
  if have_jq; then
    printf '%s' "$body" | jq -r \
      '.agents[] | "  \(.id)  \(.name)  port=\(.port // "-")  enabled=\(.enabled)  last_ip=\(.last_ip // "-")  seen=\(.last_seen // "-")"'
  elif have_py; then
    printf '%s' "$body" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for it in d.get("agents") or []:
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
  list_agents || true
  [ "$quiet" = quiet ] && return 0
  printf '\n'
}

find_agent_id(){ # name -> id ("" if not found)
  local name="$1" body
  body="$(admin_req GET /api/admin/agents)"
  if have_jq; then
    printf '%s' "$body" | jq -r --arg n "$name" \
      '.agents[] | select(.name == $n) | .id' | head -n 1
  elif have_py; then
    printf '%s' "$body" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for it in d.get("agents") or []:
    if it.get("name") == sys.argv[1]:
        print(it.get("id","")); break' "$name"
  fi
}

resolve_id(){ # name-or-id -> id
  local ref="$1" id
  case "$ref" in
    beacon|beacons) die "the beacon is gone — the coordinator detects this machine's public IP itself" ;;
  esac
  id="$(find_agent_id "$ref")"
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

cmd_rotate(){ # [agent name-or-id]
  banner
  local ref="${1:-}" id out tok
  [ -n "$ref" ] || { list_agents; prompt "Which agent (name or id)"; ref="$REPLY"; }
  [ -n "$ref" ] || die "an agent name or id is required"
  id="$(resolve_id "$ref")"
  confirm "Rotate the token for agent '$ref'? The old token stops working immediately." n
  [ "$REPLY" = y ] || { info "aborted"; return 0; }
  out="$(admin_req POST "/api/admin/agents/$id/rotate")"
  tok="$(printf '%s' "$out" | json_str token)"
  good "token rotated"
  show_token_once "$tok"
  # Keep the local service config in sync when we own it here.
  local f
  f="$(env_file agent)"
  if [ -r "$f" ] && grep -q '^IPCOD_TOKEN=' "$f" 2>/dev/null; then
    confirm "Write the new token to $f and restart the agent?" y
    if [ "$REPLY" = y ]; then
      env_set "$f" IPCOD_TOKEN "$tok"
      good "updated $f"
      svc_restart agent
      return 0
    fi
  fi
  printf '  Update the agent config with the new token, then restart it.\n\n'
}

cmd_toggle(){ # on|off <agent name-or-id>
  local state="$1" ref="${2:-}" id enabled
  banner
  [ -n "$ref" ] || { list_agents; prompt "Which agent (name or id)"; ref="$REPLY"; }
  [ -n "$ref" ] || die "an agent name or id is required"
  [ "$state" = on ] && enabled=true || enabled=false
  id="$(resolve_id "$ref")"
  admin_req PATCH "/api/admin/agents/$id" "{\"enabled\":$enabled}" >/dev/null
  good "agent '$ref' is now $state"
}

cmd_delete(){ # [agent name-or-id]
  local ref="${1:-}" id
  banner
  [ -n "$ref" ] || { list_agents; prompt "Which agent (name or id)"; ref="$REPLY"; }
  [ -n "$ref" ] || die "an agent name or id is required"
  confirm "Delete agent '$ref' and revoke its token? This cannot be undone." n
  [ "$REPLY" = y ] || { info "aborted"; return 0; }
  id="$(resolve_id "$ref")"
  admin_req DELETE "/api/admin/agents/$id" >/dev/null
  good "agent '$ref' deleted"
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
    confirm "Force the laptop IP to $new (overrides automatic detection until the IP next changes)?" n
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
    sys_ctl system disable --now "$(unit_name "$c")" >/dev/null 2>&1 || true
    good "stopped $(unit_name "$c")"
  else
    dev_stop "$c"
    unit_installed "$c" && sys_ctl system disable --now "$(unit_name "$c")" >/dev/null 2>&1 || true
  fi
  if unit_installed "$c"; then
    as_root rm -f "$(unit_file "$c")"
    local wd
    wd="$(web_dropin "$c")"
    if [ -n "$wd" ]; then
      as_root rm -f "$wd"
      as_root rmdir "$(dirname "$wd")" 2>/dev/null || true
    fi
    as_root systemctl daemon-reload
    as_root systemctl reset-failed "$(unit_name "$c")" 2>/dev/null || true
    good "removed unit $(unit_name "$c")"
  fi
  local f="$(env_file "$c")"
  if [ -f "$f" ]; then
    if [ -w "$f" ]; then rm -f "$f"; else as_root rm -f "$f"; fi
    good "removed config $f"
  fi
  # Sweep the other mode's config too (a dev uninstall must not leave the
  # systemd env behind, or vice versa), including installer .bak backups.
  local sysf devf
  sysf="$COORD_ENV"; [ "$c" = agent ] && sysf="$AGENT_ENV"
  devf="$STATE_DIR/$c.env"
  if [ "$f" != "$sysf" ] && [ -e "$sysf" ]; then
    as_root rm -f "$sysf" "$sysf".bak.*
    good "removed config $sysf"
  fi
  if [ "$f" != "$devf" ] && [ -e "$devf" ]; then
    rm -f "$devf" "$devf".bak.*
    good "removed config $devf"
  fi
  case "$c" in
    coordinator|agent) as_root rm -f "/usr/local/bin/ipcod-$c" ;;
    *) die "unknown component: $c" ;;
  esac
  good "removed binary"
  if [ "$c" = coordinator ]; then
    confirm "Also delete the database (agents, IP history)?" n
    if [ "$REPLY" = y ]; then
      # /var/lib/ipcod is a symlink into systemd's private dir when the unit
      # owns it; remove both so no stale DB survives a purge. Dev mode keeps
      # its DB (plus -wal/-shm) under the state dir.
      as_root rm -rf /var/lib/ipcod /var/lib/private/ipcod
      rm -f "$STATE_DIR/ipcod.db" "$STATE_DIR"/ipcod.db-* 2>/dev/null || true
      good "removed the coordinator database"
    fi
  fi
  rm -f "$STATE_DIR/$c.pid"
  # If that emptied the config dir, take it too (fails harmlessly if non-empty).
  as_root rmdir /etc/ipcod 2>/dev/null || true
}

# ---------------------------------------------------------------------- menu
menu_loop(){
  while true; do
    banner
    printf '  %scoordinator%s %s | %sagent%s %s\n\n' \
      "$B" "$R" "$(comp_state coordinator)" \
      "$B" "$R" "$(comp_state agent)"
    printf '   %s1)%s Status overview          %s8)%s Agents (list/create)\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s2)%s Setup / install          %s9)%s Rotate agent token\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s3)%s Start                     %s10)%s Force laptop IP\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s4)%s Stop                      %s11)%s Build binaries\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s5)%s Restart                   %s12)%s Agent dry-run on/off\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s6)%s Logs                      %s13)%s Uninstall\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s7)%s Health checks             %s14)%s Web dashboard on/off\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s0)%s Quit\n\n' "$DIM" "$R"
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
      9)  ( cmd_rotate ) || true ;;
      10) ( banner; prompt "New laptop IP (empty to just show current)" ""; cmd_ip "$REPLY" ) || true ;;
      11) ( build_binaries ) || true ;;
      12) ( banner; cmd_dryrun ) || true ;;
      13) ( banner; pick_comp; uninstall_comp "$REPLY" ) || true ;;
      14) ( web_menu ) || true ;;
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
    printf '  %sAgents%s\n' "$B" "$R"; list_agents || true
    printf '\n   %s1)%s Create agent        %s4)%s Disable agent\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s2)%s Rotate token        %s5)%s Enable agent\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s3)%s Delete agent        %s6)%s Refresh\n' "$DIM" "$R" "$DIM" "$R"
    printf '   %s0)%s Back\n\n' "$DIM" "$R"
    printf 'Choice: '
    local choice
    IFS= read -r choice || choice=0
    case "$choice" in
      1) ( cmd_create_agent ) || true ;;
      2) ( cmd_rotate ) || true ;;
      3) ( cmd_delete ) || true ;;
      4) ( cmd_toggle off ) || true ;;
      5) ( cmd_toggle on ) || true ;;
      6) : ;;
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
${B}ipcod manage.sh${R} — set up and operate the coordinator and agent.

${B}Usage${R}
  ./manage.sh                     interactive menu
  ./manage.sh [-y] <command> [args]    scriptable mode (-y pre-answers confirms with yes)

${B}Env${R}
  IPCOD_URL=http://host:port       base URL for admin API calls (default: local coordinator)
  IPCOD_ADMIN_TOKEN=token          admin token when the local env file isn't readable
  IPCOD_COORDINATOR=url            derive the API base URL from a ws URL
  IPCOD_YES=1                      same as -y
  IPCOD_DEV=1                      force plain background processes (skip systemd)
  IPCOD_STATE_DIR=path             dev-mode state dir (default ~/.local/state/ipcod)
  COORD/TOKEN/NAME/DRY_RUN/WEB_PORT   non-interactive setup defaults

${B}Commands${R}
  status                          services + coordinator summary
  setup      <comp>               install/configure a component
  start|stop|restart <comp>       control a component
  logs       <comp> [-f]          tail logs (journalctl or dev log)
  config     <comp> [show|edit]   view or edit a component's config
  health                          run health checks (healthz, agent check, public IP)
  build                           build the binaries into dist/
  list                            list registered agents (admin API)
  create-agent <name> <port> [proto]   create an agent, print its token once
  rotate      [name|id]           rotate an agent token
  enable|disable <name|id>        toggle an agent
  delete      <name|id>           delete an agent
  web         [port|off|on|status]  web dashboard, e.g. web 8443 / web off
  ip          [addr]              show the laptop IP, or force-set it
  token       admin               generate a new admin token
  dry-run     on|off              toggle the agent's ufw dry-run mode
  uninstall   <comp>              remove a component (asks before purging the DB)
  version                         print binary versions
  help                            this text

${B}Components${R}
  coordinator | agent             (abbreviations c / a work too)

${B}Examples${R}
  ./manage.sh                                  # menu
  sudo ./manage.sh setup coordinator           # then setup agent
  ./manage.sh web 8443                         # dashboard at http://127.0.0.1:8443/
  ./manage.sh create-agent web-1 22
  ./manage.sh logs agent -f
  ./manage.sh dry-run off                      # go live with real ufw changes
EOF
}

cmd_version(){
  local b bin
  for b in ipcod-coordinator ipcod-agent ipcod; do
    if [ "$b" = ipcod ]; then
      # the unified bundle only exists in dist/ (never installed to a system path)
      bin="$DIST/ipcod"
    else
      bin="$(comp_bin "${b#ipcod-}")"
    fi
    if [ -x "$bin" ]; then
      printf '  %-18s %s\n' "$b" "$("$bin" version 2>/dev/null | head -n 1)"
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
    create-beacon)       die "the beacon is gone — the coordinator detects this machine's public IP itself" ;;
    web)                 cmd_web "${1:-}" ;;
    rotate)              case "${1:-}" in agent|a) shift ;; esac; cmd_rotate "${1:-}" ;;
    enable)              case "${1:-}" in agent|a) shift ;; esac; cmd_toggle on "${1:-}" ;;
    disable)             case "${1:-}" in agent|a) shift ;; esac; cmd_toggle off "${1:-}" ;;
    delete)              case "${1:-}" in agent|a) shift ;; esac; cmd_delete "${1:-}" ;;
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
