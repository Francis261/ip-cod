# ipcod — dynamic IP allowlisting for your VPSes

ipcod keeps a set of VPS firewalls (via `ufw`) in sync with your laptop's
**current public IP**. When your IP changes, the coordinator pushes the new IP
to every connected agent, and each agent updates its firewall to allow traffic
from that IP to a specific port.

The intended use: you SSH into your VPSes from a laptop with a changing IP
(home/office/4G/hotel). Instead of opening port 22 to the world or manually
updating `ufw` every time you move, you let ipcod lock each VPS down to only
your current IP.

## How it works (3 components)

```
  ┌────────────┐   POST /api/beacon (every 30s)   ┌──────────────┐
  │  Beacon    │ ────────────────────────────────▶ │              │
  │ (laptop)   │   "here's my source IP"           │ Coordinator  │
  └────────────┘                                   │  (server)    │
                                                   │  + dashboard │
  ┌────────────┐   WebSocket /ws (persistent)      │              │
  │  Agent     │ ◀─────────────────────────────── │              │
  │ (each VPS) │   "laptop IP is now X — update   └──────────────┘
  │  + ufw     │    your firewall"
  └────────────┘
```

1. **Coordinator** — a central server you run somewhere with a stable address
   (a small VPS, your home server, a $5 DO droplet). It:
   - accepts **outbound WebSocket** connections from agents (authed with a
     per-agent bearer token),
   - receives periodic **beacon** check-ins to learn your laptop's public IP,
   - **detects IP changes** and broadcasts them to all connected agents,
   - serves an **admin dashboard** and REST API to manage agents/beacons and
     monitor state.

2. **Agent** — a small daemon installed on each VPS. It dials the coordinator
   (outbound only, so no inbound ports are needed on the VPS to reach the
   coordinator) and, on each IP change, runs the right `ufw` commands to allow
   the new laptop IP → port, removing the old rule.

3. **Beacon** — a tiny process on your laptop that POSTs to the coordinator
   every ~30s. The coordinator reads the request's **source IP**; when it
   changes, that's the trigger.

**Why agents dial out:** VPSes can almost always make outbound connections,
even when inbound is filtered. A pull model means you open zero inbound ports
on the VPS just to reach the coordinator.

**Why a beacon:** the coordinator can't know your laptop's IP unless something
tells it. A ~50-line client that POSTs periodically is the simplest reliable
signal, and it works across any network you're on.

## Security model

- **Tokens are bearer secrets**, `ipca_…` (agent) and `ipcb_…` (beacon),
  generated from 256 bits of CSPRNG entropy. **Only a SHA-256 hash is stored**
  in the coordinator DB, so a DB leak doesn't leak usable tokens.
- **Agent WebSocket auth:** the first frame must be an `auth` message. Wrong
  token type (e.g. a beacon token), unknown token, or a disabled agent → the
  connection is dropped. Beacon tokens are rejected on the agent endpoint.
- **Admin API** is guarded by a separate admin bearer token; the dashboard is
  just a UI over it. Set `ADMIN_TOKEN` or the admin API is **disabled
  (fail-closed)**.
- **Revision counter** on IP updates: each change increments a monotonic
  revision, and agents ignore any update with a revision ≤ the last one they
  applied, preventing stale/replayed updates.
- **`ufw` rules are scoped and replaced, not accumulated.** The agent tracks
  the single rule it applied and, on change, deletes the old one before adding
  the new. It tags rules with a `ipcod` comment so you can find them in
  `ufw status`. **The delete-before-add order matters:** it never leaves a
  window where a stale IP is still allowed (it briefly allows neither, which is
  the safe direction).

> ⚠️ **Read before pointing this at SSH (port 22).** If the coordinator is
> unreachable or your beacon stops, the agent simply **keeps the last rule** —
> it does not tear down access on a lapse. But if you rotate/delete an agent or
> the IP pushes a wrong value, you can lock yourself out of SSH. Mitigations
> built in: (1) run with a **second, backup access path** (console/VNC via your
> provider) the first time you trust it; (2) test with `DRY_RUN=1` first;
> (3) start by guarding a **non-SSH** port. The agent never touches rules it
> didn't create.

## TL;DR — run it locally in 3 minutes

```bash
make build      # builds dist/{ipcod-coordinator,ipcod-agent,ipcod-beacon,ipcod}
make test       # vet + unit tests

# 1. Start a coordinator on localhost
IPCOD_ADMIN_TOKEN=devadmin ./dist/ipcod-coordinator run \
  -listen 127.0.0.1:8443 -db /tmp/ipcod.db
# ...or via the unified binary: ./dist/ipcod coordinator run -listen ... 

# 2. In the browser: open http://127.0.0.1:8443, sign in with "devadmin",
#    create an agent (name + port) and a beacon (name). Copy each token
#    (shown once).

# 3. Start an agent in dry-run (no real ufw changes) + a beacon:
IPCOD_COORDINATOR=ws://127.0.0.1:8443/ws \
IPCOD_TOKEN=ipca_xxx ./dist/ipcod-agent run -dry-run -v

IPCOD_COORDINATOR=http://127.0.0.1:8443/api/beacon \
IPCOD_TOKEN=ipcb_yyy ./dist/ipcod-beacon run -v
```

Watch the agent log print `firewall updated …` and, in dry-run, the exact
`ufw allow from <ip> to any port <port> proto tcp comment ipcod` it would run.

## manage.sh — one script to run it all

`manage.sh` at the repo root is a menu-driven front door for all three
components. Run it with no arguments for the menu, or pass a command to script
it:

```bash
./manage.sh                      # interactive menu (status/setup/start/…)

sudo ./manage.sh setup coordinator     # delegates to deploy/install-coordinator.sh
COORD=wss://coord.example.com/ws TOKEN=ipca_xxx ./manage.sh setup agent
COORD=coord.example.com TOKEN=ipcb_yyy ./manage.sh setup beacon   # laptop, systemd --user

./manage.sh status               # services + current IP + connected agents
./manage.sh health               # healthz + `agent check` + `beacon once`
./manage.sh logs agent -f        # journalctl (or the dev log) for a component
./manage.sh restart beacon
./manage.sh create-agent web-1 22        # via the admin API; token shown once
./manage.sh -y rotate agent web-1        # rotates AND rewrites the local env + restarts
./manage.sh dry-run on           # flip the agent between log-only and live ufw
./manage.sh -y ip 203.0.113.7    # force-push an IP
./manage.sh -y uninstall coordinator     # asks first; DB purge is a second question
```

Notes:

- **systemd when available, processes otherwise.** On a systemd host it drives
  `ipcod-coordinator` / `ipcod-agent` (system units) and `ipcod-beacon`
  (user unit, with optional lingering). Without systemd — or with
  `IPCOD_DEV=1` — it runs plain background processes with pidfiles and logs
  under `~/.local/state/ipcod`, which is handy for trying it out locally.
- **`setup` reconfigures too**, not just installs: it pre-fills every prompt
  from the existing config and restarts the service so changes take effect.
- **`-y` / `IPCOD_YES=1`** pre-answers the destructive confirmations, so the
  script is usable from cron/CI. Tokens are still printed exactly once.
- **Admin API calls** go to the local coordinator's loopback address by
  default; override with `IPCOD_URL=…` or `IPCOD_ADMIN_TOKEN=…` to drive a
  remote coordinator from your laptop.
- `./manage.sh help` prints the full command reference.

## Production deployment

### 1. Coordinator (with TLS)

Put the coordinator behind a TLS-terminating reverse proxy so agents use
`wss://` and beacons use `https://`. Example Caddy config:

```
coord.example.com {
    reverse_proxy 127.0.0.1:8443
}
```

Then install and start:

```bash
make build
sudo ADMIN_TOKEN="$(openssl rand -base64 32)" ./deploy/install-coordinator.sh
# -> prints the generated ADMIN_TOKEN; save it. It's in /etc/ipcod/coordinator.env.
```

If you're behind the proxy, set `TRUST_PROXY=1` in
`/etc/ipcod/coordinator.env` so the coordinator trusts `X-Forwarded-For` for
beacon source IPs, then `sudo systemctl restart ipcod-coordinator`.

Open `https://coord.example.com`, sign in with the admin token, and create your
agents and a beacon.

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

### 3. Beacon (on your laptop)

The beacon is a user-level process. On Linux with systemd, `manage.sh` can set
it up for you (writes `~/.config/ipcod-beacon.env`, installs the user unit,
optionally enables lingering):

```bash
COORD=coord.example.com TOKEN=ipcb_yyy ./manage.sh setup beacon
```

Or do it by hand:

```bash
mkdir -p ~/.config/systemd/user ~/.local/bin
cp dist/ipcod-beacon ~/.local/bin/ipcod-beacon
cp deploy/ipcod-beacon.service ~/.config/systemd/user/
cp deploy/beacon.env.example ~/.config/ipcod-beacon.env   # edit: COORDINATOR + TOKEN
systemctl --user daemon-reload
systemctl --user enable --now ipcod-beacon
loginctl enable-linger "$USER"     # keep running while logged out
```

Or run it by hand / via launchd on macOS (see `dist/ipcod-beacon`; it's a single
static binary). To verify your token + URL in one shot:

```bash
./dist/ipcod-beacon once -coordinator https://coord.example.com/api/beacon -token ipcb_yyy
# -> ip=203.0.113.7 changed=true
```

## Dashboard

Served at the coordinator root. Sign in with the admin bearer token. It shows:

- **Current laptop IP**, revision, last-changed time, and how many agents are
  connected. Includes a **"Force IP"** override to push a specific IP manually.
- **Agents table** — per-agent port, live connected/offline status, last IP
  applied, last seen, plus **create / rotate-token / delete**.
- **Beacons table** — last IP, last seen, enable/disable, rotate, delete.
- **Recent IP history** — audit trail of changes.

## REST API

All under `/api/admin/*`, guarded by `Authorization: Bearer <ADMIN_TOKEN>`.

| Method & path | Purpose |
|---|---|
| `GET  /api/admin/status` | current IP, revision, connected-agent count |
| `POST /api/admin/ip` | force-set the laptop IP `{"ip":"1.2.3.4"}` |
| `GET  /api/admin/ip-history?limit=N` | recent IP changes |
| `GET  /api/admin/agents` | list agents (with live status) |
| `POST /api/admin/agents` | create agent `{name,port,proto}` → returns token **once** |
| `PATCH /api/admin/agents/{id}` | update `{name?,port?,proto?,enabled?}` |
| `POST /api/admin/agents/{id}/rotate` | rotate token → new token **once** |
| `DELETE /api/admin/agents/{id}` | delete agent |
| `GET/POST /api/admin/beacons`, `PATCH/DELETE/…/{id}`, `…/{id}/rotate` | beacon CRUD |

Beacon check-in (bearer = the beacon's own token, not the admin token):

| Method & path | Purpose |
|---|---|
| `POST /api/beacon` | report source IP; returns `{ip, changed, revision}` |

## Configuration reference

**Coordinator** — flags or env (`IPCOD_*`):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-listen` | `IPCOD_LISTEN` | `:8443` | HTTP bind address |
| `-db` | `IPCOD_DB` | `ipcod.db` | SQLite path |
| `-admin-token` | `IPCOD_ADMIN_TOKEN` | — | admin bearer token (empty ⇒ admin API off) |
| `-trust-proxy` | `IPCOD_TRUST_PROXY` | `0` | trust `X-Forwarded-For` (behind proxy) |
| `-v` | — | `false` | debug logging |

**Agent** — flags or env:

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-coordinator` | `IPCOD_COORDINATOR` | — | `ws://`/`wss://…/ws` (required) |
| `-token` | `IPCOD_TOKEN` | — | agent token (required) |
| `-name` | `IPCOD_NAME` | hostname | display name |
| `-ufw` | `IPCOD_UFW` | `ufw` | ufw binary path |
| `-dry-run` | `IPCOD_DRY_RUN` | `0` | log instead of running ufw |
| `-v` | — | `false` | debug logging |

**Beacon:**

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-coordinator` | `IPCOD_COORDINATOR` | — | `http(s)://…/api/beacon` (required) |
| `-token` | `IPCOD_TOKEN` | — | beacon token (required) |
| `-interval` | — | `30s` | check-in interval |
| `-v` | — | `false` | debug logging |

Use `ipcod-beacon once` to report a single time and exit (no `-once` flag).

## Command-line interface

Every role ships as a **single static binary** with a small subcommand CLI:

| Binary | Subcommands | Notes |
|---|---|---|
| `ipcod-coordinator` | `run` (default), `gen-admin-token`, `version` | |
| `ipcod-agent` | `run` (default), `check`, `version` | `check` = verify connectivity + auth, then exit |
| `ipcod-beacon` | `run` (default), `once`, `version` | `once` = report one IP and exit |
| `ipcod` | `coordinator`, `agent`, `beacon`, `version` | unified bundle of all three |

The unified `ipcod` binary exposes the same three roles as nested subcommands,
so `ipcod agent run -dry-run` is identical to `ipcod-agent run -dry-run`. Use
whichever you prefer — the role binaries are just the same code with the role
pinned as the root command.

Every command supports `--help` (or `help`), which prints the full invocation
path, available subcommands, and flags:

```bash
ipcod --help
ipcod agent --help          # lists run / check / version
ipcod agent check --help    # shows check's flags (-coordinator, -token, -timeout, …)
ipcod-agent version         # -> ipcod-agent v1.2.3
```

Useful one-shot commands:

```bash
# Validate an agent's token + coordinator URL without starting the daemon:
ipcod-agent check -coordinator wss://coord.example.com/ws -token ipca_xxx

# Report the laptop IP once and exit (good for cron/testing):
ipcod-beacon once -coordinator https://coord.example.com/api/beacon -token ipcb_yyy

# Generate an admin token for the coordinator:
ipcod-coordinator gen-admin-token
```

## Project layout

```
manage.sh          interactive/scriptable lifecycle manager for all three roles
cmd/ipcod           unified binary (coordinator/agent/beacon as subcommands)
cmd/coordinator     standalone coordinator binary
cmd/agent           standalone agent binary
cmd/beacon          standalone beacon binary
internal/cli        subcommand framework + per-role command definitions
internal/protocol   WebSocket message envelope + types
internal/token      token generation + hashing (SHA-256, constant-time verify)
internal/store      SQLite persistence (agents, beacons, IP history)
internal/coordinator  WS server, connection hub, admin API, dashboard
internal/agent      WS client, reconnect/backoff, ufw applier
internal/beacon     laptop IP reporter
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
of `ipcod`, `ipcod-coordinator`, `ipcod-agent`, `ipcod-beacon`, plus
`dist/SHA256SUMS`.

## Design notes & tradeoffs

- **Single coordinator, no HA.** SQLite in WAL mode handles many agents
  trivially. If you outgrow one node, move `internal/store` to Postgres and run
  multiple coordinators behind a sticky LB (agents reconnect and reconcile on
  connect, so this is a natural extension).
- **Agent keeps last rule on coordinator loss.** By design, an unreachable
  coordinator is a no-op — the firewall stays as-is. Availability of your
  access is preferred over aggressive re-locking.
- **Reconciliation on connect.** When an agent (re)connects, the coordinator
  immediately pushes the current IP, so an agent that was down during a change
  catches up without waiting for the next change.
