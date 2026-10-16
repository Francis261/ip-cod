# ipcod — dynamic IP allowlisting for your VPSes

ipcod keeps a set of VPS firewalls (via `ufw`) in sync with your **current
public IP**. When your IP changes, the coordinator pushes the new IP to every
connected agent, and each agent updates its firewall to allow traffic from
that IP to a specific port.

The intended use: you SSH into your VPSes from a machine with a changing IP
(home/office/4G/hotel). Instead of opening port 22 to the world or manually
updating `ufw` every time you move, you let ipcod lock each VPS down to only
your current IP.

## How it works (2 components)

```
          HTTPS, every 60s: "what is my IP?"
   ┌────────────┐ ─────────────────────────────▶ ┌──────────────────────────┐
   │ echo       │   (majority vote among several │ ipify, icanhazip,        │
   │ services   │    independent services)        │ ifconfig.me, ipinfo, …   │
   └────────────┘                                └──────────────────────────┘
                                                            │
                                                            ▼ detected IP changed?
   ┌───────────────────────────────────────────────────────────────────────┐
   │  Coordinator — run it on YOUR LAPTOP (or any machine whose IP          │
   │  is the one you want allowed)                                         │
   │                                                                       │
   │   • discovers this machine's public IP itself (no helper process)     │
   │   • pushes changes to every connected agent, and re-asserts the       │
   │     current IP+ports to them every 30s (self-heal)                    │
   │   • admin API + web dashboard, served on demand (`web -p 8443`)       │
   └───────────────────────────────▲───────────────────────────────────────┘
                                   │ WebSocket /ws (persistent; the AGENT
                                   │ dials out, so it needs no inbound port)
                                   │ "public IP is now X — update your rule"
   ┌───────────────────────────────┴───────────────────────────────────────┐
   │  Agent — one per VPS, manages ufw                                     │
   │   • connects out to the coordinator                                   │
   │   • on each push: ensure allow <current IP> → <ports>, replacing      │
   │     rules that no longer match (missing ones are re-added)            │
   └───────────────────────────────────────────────────────────────────────┘
```

1. **Coordinator** — run it on your laptop (that's where the changing IP is).
   It:
   - determines **this machine's public IP** by asking several echo services
     outbound over HTTPS and taking the majority answer, re-checking every
     minute — nothing else needs to run on your laptop,
   - accepts **outbound WebSocket** connections from agents (authed with a
     per-agent bearer token), broadcasts IP changes, and re-asserts the
     current IP+ports to every connected agent every 30 seconds (so a rule
     deleted out-of-band gets repaired without waiting for a change),
   - serves an **admin dashboard and REST API** — the dashboard only when you
     ask for it: `ipcod-coordinator web -p 8443` (plain `run` is headless).

2. **Agent** — a small daemon installed on each VPS. It dials the coordinator
   (outbound only, so no inbound ports are needed on the VPS to reach the
   coordinator) and, on each push, converges `ufw` to exactly the right state:
   allow the current laptop IP → its configured **ports** (one agent can guard
   several, e.g. `22,443`), replacing rules that no longer match and re-adding
   missing ones.

**Why agents dial out:** VPSes can almost always make outbound connections,
even when inbound is filtered. A pull model means you open zero inbound ports
on the VPS just to reach the coordinator.

**Why the coordinator finds the IP itself:** the process that needs to react
to your IP changing is already running where that IP lives. It asks echo
services outbound (like `curl https://api.ipify.org` does), so there is no
second process, unit, or token to manage on your laptop.

> **Reachability is your one operational caveat.** Agents must be able to
> *dial in* to the coordinator. If the laptop sits behind NAT/CGNAT (typical
> home/4G), give it one of: a **public IP**, a **Tailscale/WireGuard address**
> (agents on the tailnet dial `wss://100.x.y.z:8443/ws` — works through CGNAT),
> or a **tunnel** (Cloudflare Tunnel / Caddy) on a hostname, which also gets
> you TLS. On a laptop you control, none of these need root access to the VPS.

**Legacy:** `POST /api/beacon` still exists for older `ipcod-beacon` binaries
(a tiny client that reported the laptop IP to a remote coordinator). New
deployments don't need it; the endpoint and its dashboard card are labelled
"legacy".

## Security model

- **Tokens are bearer secrets**, `ipca_…` (agent) and `ipcb_…` (legacy beacon),
  generated from 256 bits of CSPRNG entropy. **Only a SHA-256 hash is stored**
  in the coordinator DB, so a DB leak doesn't leak usable tokens.
- **Agent WebSocket auth:** the first frame must be an `auth` message. Wrong
  token type (e.g. a beacon token), unknown token, or a disabled agent → the
  connection is dropped. Beacon tokens are rejected on the agent endpoint.
- **Admin API** is guarded by a separate admin bearer token; the dashboard is
  just a UI over it. Set `ADMIN_TOKEN` or the admin API is **disabled
  (fail-closed)**.
- **Revision counter** on IP updates: each change increments a monotonic
  revision, and agents ignore any update with a revision **older** than the
  last one they saw, preventing stale/replayed updates. Equal revisions are
  the coordinator's periodic re-assertions of the *current* state — safe to
  process, and the agent just re-verifies its rules (a no-op when they match).
- **`ufw` rules are scoped and replaced, not accumulated.** The agent tags
  every rule it creates with an `ipcod` comment and only ever manages rules
  carrying that tag — it never touches rules you added yourself. On each push
  it compares `ufw status` against the desired state and touches only the
  difference. **Deletes run before adds**, so there is never a window where a
  stale IP is still allowed (it briefly allows neither, which is the safe
  direction).

> ⚠️ **Read before pointing this at SSH (port 22).** If the coordinator is
> unreachable, the agent simply **keeps the last rule** — it does not tear
> down access on a lapse. (A *clean* stop of the agent — `systemctl stop` or a
> host reboot — does remove its rule; the agent re-adds it within seconds of
> reconnecting.) But if you rotate/delete an agent or the IP pushes a
> wrong value, you can lock yourself out of SSH. Mitigations built in: (1) run
> with a **second, backup access path** (console/VNC via your provider) the
> first time you trust it; (2) test with `DRY_RUN=1` first; (3) start by
> guarding a **non-SSH** port. The agent never touches rules it didn't create.

## TL;DR — run it locally in 3 minutes

```bash
make build      # builds dist/{ipcod-coordinator,ipcod-agent,ipcod}
make test       # vet + unit tests

# 1. Start a coordinator with the dashboard on localhost. `run` (no dashboard)
#    is the headless default; `web -p <port>` adds the UI.
IPCOD_ADMIN_TOKEN=devadmin ./dist/ipcod-coordinator web -p 8443 \
  -listen 127.0.0.1:8443 -db /tmp/ipcod.db
# ...or via the unified binary: ./dist/ipcod coordinator web -p 8443 ...

# 2. In the browser: open http://127.0.0.1:8443, sign in with "devadmin",
#    create an agent (name + ports, e.g. 22,443), copy the token (shown once).
#    The dashboard shows the public IP this machine detected for itself.

# 3. Start an agent in dry-run (no real ufw changes):
IPCOD_COORDINATOR=ws://127.0.0.1:8443/ws \
IPCOD_TOKEN=ipca_xxx ./dist/ipcod-agent run -dry-run -v
```

Watch the agent log print `firewall updated …` and, in dry-run, the exact
`ufw allow from <ip> to any port <port> proto tcp comment ipcod` it would run.

## manage.sh — one script to run it all

`manage.sh` at the repo root is a menu-driven front door for both components.
Run it with no arguments for the menu, or pass a command to script it:

```bash
./manage.sh                      # interactive menu (status/setup/start/…)

sudo ./manage.sh setup coordinator     # delegates to deploy/install-coordinator.sh
COORD=wss://coord.example.com/ws TOKEN=ipca_xxx ./manage.sh setup agent

./manage.sh status               # services + current IP + connected agents
./manage.sh health               # healthz + `agent check` + detected public IP
./manage.sh logs agent -f        # journalctl (or the dev log) for a component
./manage.sh web 8443             # dashboard on http://127.0.0.1:8443/
./manage.sh web off              # back to headless (coordinator keeps running)
./manage.sh create-agent web-1 22,443   # via the admin API; token shown once
./manage.sh -y rotate web-1              # rotates AND rewrites the local env + restarts
./manage.sh dry-run on           # flip the agent between log-only and live ufw
./manage.sh -y ip 203.0.113.7    # force-push an IP
./manage.sh -y uninstall coordinator     # asks first; DB purge is a second question
```

Notes:

- **systemd when available, processes otherwise.** On a systemd host it drives
  `ipcod-coordinator` / `ipcod-agent` (system units). Without systemd — or
  with `IPCOD_DEV=1` — it runs plain background processes with pidfiles and
  logs under `~/.local/state/ipcod`, which is handy for trying it out locally.
- **`web` under systemd** writes a unit drop-in that swaps `ExecStart` to
  `web -p <port>`; in dev mode it records `IPCOD_WEB_PORT` in the dev config.
  Either way it restarts the coordinator, so `web off` is a live toggle.
- **`setup` reconfigures too**, not just installs: it pre-fills every prompt
  from the existing config and restarts the service so changes take effect.
- **`-y` / `IPCOD_YES=1`** pre-answers the destructive confirmations, so the
  script is usable from cron/CI. Tokens are still printed exactly once.
- **Admin API calls** go to the local coordinator's loopback address by
  default; override with `IPCOD_URL=…` or `IPCOD_ADMIN_TOKEN=…` to drive a
  remote coordinator from your laptop.
- `./manage.sh help` prints the full command reference.

## Production deployment

### 1. Coordinator (the laptop side)

```bash
make build
sudo ADMIN_TOKEN="$(openssl rand -base64 32)" ./deploy/install-coordinator.sh
# -> prints the generated ADMIN_TOKEN; save it. It's in /etc/ipcod/coordinator.env.
# The installer asks whether to serve the web dashboard (port included); if you
# decline, start it later with `ipcod-coordinator web -p 8443` or
# `sudo ./manage.sh web 8443`.
```

The coordinator polls public echo services for this machine's IP every 60s
(`IPCOD_DETECT_INTERVAL`, `IPCOD_IP_PROVIDERS`, `IPCOD_NO_DETECT` — see the
config reference).

**Agents need to reach it.** Depending on where it runs:

- **Machine with a public IP/hostname + TLS (Caddy):**

  ```
  coord.example.com {
      reverse_proxy 127.0.0.1:8443
  }
  ```

  Agents then use `wss://coord.example.com/ws`.
- **Laptop behind NAT/CGNAT:** run it on the tailnet — agents dial the
  coordinator's Tailscale IP directly (`wss://100.x.y.z:8443/ws`) — or put a
  Cloudflare tunnel in front of `127.0.0.1:8443` and point agents at
  `wss://tunnel.example.com/ws`.

If you're behind a reverse proxy and still use the **legacy beacon endpoint**,
set `TRUST_PROXY=1` in `/etc/ipcod/coordinator.env` so check-in source IPs
come from `X-Forwarded-For` (outbound detection doesn't use it), then
`sudo systemctl restart ipcod-coordinator`.

### 2. Agent (on each VPS)

```bash
make build
sudo COORD=wss://coord.example.com/ws TOKEN=ipca_xxx ./deploy/install-agent.sh
# add DRY_RUN=1 first if you want to confirm behavior without touching ufw.
```

Confirm in the dashboard that the agent shows **connected**, then check the VPS:

```bash
sudo ufw status | grep ipcod     # should show the allow rule for your IP
journalctl -u ipcod-agent -f
```

### 3. Nothing else on the laptop

There is no beacon to install — the coordinator itself reports the machine's
public IP. If you have an **older** setup where the coordinator runs on a
server, an old `ipcod-beacon` binary on the laptop keeps working against the
retained `POST /api/beacon` endpoint.

## Dashboard

Served at the coordinator root when you start it with `web` (`run` stays
headless and answers `/` with a short hint instead). Sign in with the admin
bearer token. It shows:

- **Current public IP**, revision, last-changed time, and how many agents are
  connected. Includes a **"Force IP"** override to push a specific IP manually.
- **Agents table** — per-agent ports, live connected/offline status, last IP
  pushed, last seen, plus **create / edit / rotate-token / delete** (the edit
  dialog changes name, ports, proto, or enabled; port changes are pushed to a
  connected agent immediately).
- **Beacon override (legacy)** — only needed for old `ipcod-beacon` binaries.
- **Recent IP history** — audit trail of changes, with the source (`detected`
  for automatic detection, a beacon name for legacy check-ins).

## REST API

All under `/api/admin/*`, guarded by `Authorization: Bearer <ADMIN_TOKEN>`.

| Method & path | Purpose |
|---|---|
| `GET  /api/admin/status` | current IP, revision, connected-agent count |
| `POST /api/admin/ip` | force-set the public IP `{"ip":"1.2.3.4"}` |
| `GET  /api/admin/ip-history?limit=N` | recent IP changes |
| `GET  /api/admin/agents` | list agents (with live status) |
| `POST /api/admin/agents` | create agent `{name,ports:[…],proto}` → returns token **once** |
| `PATCH /api/admin/agents/{id}` | update `{name?,ports?,port?,proto?,enabled?}` |
| `POST /api/admin/agents/{id}/rotate` | rotate token → new token **once** |
| `DELETE /api/admin/agents/{id}` | delete agent |
| `GET/POST /api/admin/beacons`, `PATCH/DELETE/…/{id}`, `…/{id}/rotate` | legacy beacon CRUD |

Legacy beacon check-in (bearer = the beacon's own token, not the admin token):

| Method & path | Purpose |
|---|---|
| `POST /api/beacon` | report source IP; returns `{ip, changed, revision}` |

## Configuration reference

**Coordinator** — flags or env (`IPCOD_*`):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-listen` | `IPCOD_LISTEN` | `:8443` | HTTP bind address |
| `-p` / `-port` | — | — | `web` only: port to serve (overrides the port in `-listen`) |
| `-db` | `IPCOD_DB` | `ipcod.db` | SQLite path |
| `-admin-token` | `IPCOD_ADMIN_TOKEN` | — | admin bearer token (empty ⇒ admin API off) |
| `-trust-proxy` | `IPCOD_TRUST_PROXY` | `0` | trust `X-Forwarded-For` (legacy beacon check-ins) |
| `-detect-interval` | `IPCOD_DETECT_INTERVAL` | `60s` | public-IP re-check interval |
| `-reconcile-interval` | `IPCOD_RECONCILE_INTERVAL` | `30s` | how often to re-push the current IP+ports to connected agents (self-heal; `0` disables) |
| `-providers` | `IPCOD_IP_PROVIDERS` | built-in list | comma-separated echo-service URLs |
| `-no-detect` | `IPCOD_NO_DETECT` | `0` | disable outbound detection (set IP manually / legacy beacon) |
| `-v` | — | `false` | debug logging |

Run modes: `ipcod-coordinator run` (headless) and
`ipcod-coordinator web -p 8443` (same, plus the dashboard).

**Agent** — flags or env:

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-coordinator` | `IPCOD_COORDINATOR` | — | `ws://`/`wss://…/ws` (required) |
| `-token` | `IPCOD_TOKEN` | — | agent token (required) |
| `-name` | `IPCOD_NAME` | hostname | display name |
| `-ufw` | `IPCOD_UFW` | `ufw` | ufw binary path |
| `-dry-run` | `IPCOD_DRY_RUN` | `0` | log instead of running ufw |
| `-v` | — | `false` | debug logging |

## Command-line interface

Every role ships as a **single static binary** with a small subcommand CLI:

| Binary | Subcommands | Notes |
|---|---|---|
| `ipcod-coordinator` | `run` (default), `web`, `gen-admin-token`, `version` | `web -p <port>` adds the dashboard |
| `ipcod-agent` | `run` (default), `check`, `version` | `check` = verify connectivity + auth, then exit |
| `ipcod` | `coordinator`, `agent`, `version` | unified bundle of both roles |

The unified `ipcod` binary exposes the same roles as nested subcommands, so
`ipcod coordinator web -p 8443` is identical to `ipcod-coordinator web -p
8443`. Use whichever you prefer — the role binaries are just the same code
with the role pinned as the root command.

Every command supports `--help` (or `help`), which prints the full invocation
path, available subcommands, and flags:

```bash
ipcod --help
ipcod coordinator --help        # lists run / web / gen-admin-token / version
ipcod coordinator web --help    # shows web's flags (-p, -listen, -db, …)
ipcod-agent version             # -> ipcod-agent v1.2.3
```

Useful one-shot commands:

```bash
# Validate an agent's token + coordinator URL without starting the daemon:
ipcod-agent check -coordinator wss://coord.example.com/ws -token ipca_xxx

# Generate an admin token for the coordinator:
ipcod-coordinator gen-admin-token
```

## Project layout

```
manage.sh          interactive/scriptable lifecycle manager for both roles
cmd/ipcod           unified binary (coordinator/agent as subcommands)
cmd/coordinator     standalone coordinator binary
cmd/agent           standalone agent binary
internal/cli        subcommand framework + per-role command definitions
internal/protocol   WebSocket message envelope + types
internal/token      token generation + hashing (SHA-256, constant-time verify)
internal/store      SQLite persistence (agents, IP history, legacy beacons)
internal/coordinator  WS server, connection hub, admin API, dashboard, IP detector wiring
internal/ipdetect   outbound public-IP detection (echo services + majority vote)
internal/agent      WS client, reconnect/backoff, ufw applier
deploy/             systemd units, env examples, install scripts
```

## Development

```bash
make test        # go vet + unit tests
make release     # cross-compile static linux/{amd64,arm64} binaries + SHA256SUMS
```

All binaries — including the unified `ipcod` — are `CGO_ENABLED=0` static
builds (the SQLite driver is pure Go), so they run on any Linux VPS with no
runtime deps. `make release` emits `dist/<name>-linux-{amd64,arm64}` for each
of `ipcod`, `ipcod-coordinator`, `ipcod-agent`, plus `dist/SHA256SUMS`.

## Design notes & tradeoffs

- **Detection: outbound echo, majority vote.** Every round the coordinator
  asks several independent "what is my IP" services in parallel; the
  most-reported valid answer wins, so one service going away (or lying) is
  harmless, and a failed round simply keeps the last known IP. `POST
  /api/beacon` remains as a manual/legacy override.
- **Single coordinator, no HA.** SQLite in WAL mode handles many agents
  trivially. If you outgrow one node, move `internal/store` to Postgres and run
  multiple coordinators behind a sticky LB (agents reconnect and reconcile on
  connect, so this is a natural extension).
- **Agent keeps last rule on coordinator loss.** By design, an unreachable
  coordinator is a no-op — the firewall stays as-is. Availability of your
  access is preferred over aggressive re-locking.
- **Periodic re-assertion (self-heal).** Besides pushing on change and on
  connect, the coordinator re-sends the current IP+ports to every connected
  agent every 30s (`-reconcile-interval`/`IPCOD_RECONCILE_INTERVAL`, `0`
  disables). Agents treat it as a state assertion: they diff it against
  `ufw status` and touch only the difference, so a rule deleted out-of-band is
  repaired within one interval, while an unchanged rule is a silent (debug-log)
  no-op — no log spam, no needless `ufw` writes. One read-only `ufw status`
  runs per interval to verify.
- **Reconciliation on connect.** When an agent (re)connects, the coordinator
  immediately pushes the current IP, so an agent that was down during a change
  catches up without waiting for the next change.
