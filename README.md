# Linux Dash agent

A small Go service that runs on each Linux server and connects it to the Linux Dash dashboard. It:

- **Monitors the host:** CPU, iowait, load, memory and swap, disk space and inodes per mount, network throughput and errors per interface, and uptime. Values are read directly from `/proc` and `statfs`.
- **Monitors applications:**
  - Docker containers via the Engine API socket: state, health, restarts, OOM kills, CPU and memory.
  - PM2 processes, for one or more users.
  - systemd units you choose to watch, plus any failed unit.
- **Reports errors** from journald (priority `err` and above by default), resuming from a saved cursor across restarts.
- **Manages packages** with apt, dnf/yum, apk or pacman:
  - lists pending upgrades and flags security fixes (and advisories) where the distro provides them
  - detects when a reboot is required
  - runs unattended upgrades in a maintenance window or every N hours, with a free-disk pre-flight check and optional reboot
- **Accepts remote actions** from the dashboard: restart/stop/start containers, PM2 apps and services; check or apply updates; reboot. Only a fixed set of actions is accepted, and each one can be switched off locally.

It is a single static binary (about 7 MB) for amd64, arm64, arm and 386. The only dependency is `gopkg.in/yaml.v3`.

## How it talks to the dashboard

```
 Linux host                                   Dashboard (control plane)
┌──────────────────────┐   HTTPS, outbound   ┌──────────────────────────┐
│ linux-dash-agent     │ ──── enroll ──────▶ │ POST /api/agent/v1/enroll│
│  metrics · apps      │ ──── report ──────▶ │ POST /api/agent/v1/report│
│  journal · updates   │ ◀─ commands/policy ─│                          │
└──────────────────────┘                     └──────────────────────────┘
```

The agent always dials out, so servers need no open inbound ports. Every report (default every 10s) carries fresh metrics plus anything that changed. The response carries queued commands and the update policy. The full spec is in [docs/PROTOCOL.md](docs/PROTOCOL.md).

## Security model

- **Allowlisted actions only.** There is no "run this shell command" capability. Names passed to `apt`, `docker`, `pm2` and `systemctl` are validated, so values like `--force` or `a;b` are rejected.
- **Local veto.** The `actions:` block in the agent's config decides what the dashboard may do on that host. Reboots are off by default. systemd control is limited to watched units unless `control_any: true`.
- **HTTPS required.** Plain HTTP needs an explicit `allow_insecure_http` flag (for local development). A custom CA is supported.
- **Protected credentials.** Credentials are stored in `/var/lib/linux-dash-agent/state.json` with mode `0600`.
- **Runs as root.** Package management requires it. PM2 commands are run as the owning user via `runuser`.

## Install

On a server, as root. The installer asks for everything it needs:

```bash
curl -fsSL https://dash.example.com/install.sh | sudo sh
# or, with the release files copied over (uses the binary next to the script):
sudo sh install.sh
```

It asks for:

1. **Dashboard address:** an IP or hostname (with `:port` if not 443) or a full URL. It asks whether the dashboard uses HTTPS and then checks that it's reachable. Plain HTTP needs explicit confirmation. If the certificate is self-signed, you can give the CA file or (not recommended) skip verification.
2. **Enrolment token:** typed hidden, from Settings → Agent enrolment.
3. **Remote reboots:** allow them or not (off by default).
4. **What to monitor:** systemd services (suggests the enabled ones it detects, such as nginx, postgresql or redis) and PM2 users (detects `~/.pm2` and finds nvm-installed `pm2`). Docker is picked up automatically.
5. **Automatic updates:** off, security only, or all, plus days, start time and timezone (defaults to the server's), and whether to reboot afterwards.

It then shows a summary to confirm, writes `/etc/linux-dash-agent/config.yaml` (mode `0600`), and checks that config with the agent before using it. Finally it installs the systemd unit (or an OpenRC service on Alpine) and confirms the service stays up. Re-running it on an installed server offers to keep the existing config and only upgrade the binary.

Every question has a flag for scripted installs. With `--yes`, anything not given uses its default:

```bash
sudo sh install.sh --yes --server 10.0.0.5:3000 --allow-http --token <TOKEN> \
  --watch nginx,postgresql --pm2-users deploy --auto-updates security --update-days sun --update-time 02:00
sh install.sh --dry-run      # walk through the questions and print the config; changes nothing
sh install.sh --help         # all options
```

Logs: `journalctl -u linux-dash-agent -f`. See [deploy/config.example.yaml](deploy/config.example.yaml) for every option. After editing the config, run `linux-dash-agent validate` before restarting.

### Check what the agent sees

```bash
sudo linux-dash-agent check              # one collection, printed as JSON; nothing is sent
sudo linux-dash-agent check -updates     # also refresh package lists
```

## Develop

```bash
make test          # unit + end-to-end tests (race detector)
make vet           # vet for the host and for linux
make release       # dist/linux-dash-agent-linux-{amd64,arm64,arm,386} + install.sh + SHA256SUMS
make devserver     # in-memory control plane on :8080 (enrol token: dev-token)
```

Run against the dev server:

```yaml
# dev.yaml
server: { url: http://127.0.0.1:8080, enroll_token: dev-token, allow_insecure_http: true }
state_dir: ./.state
intervals: { report: 2s }
```

```bash
go run ./cmd/linux-dash-agent run -config dev.yaml
curl localhost:8080/dev/agents                                   # list agents
curl -X POST localhost:8080/dev/agents/<id>/commands \
     -d '{"type":"docker.restart","args":{"id":"nginx"}}'         # queue a command
curl -X PUT localhost:8080/dev/agents/<id>/policy \
     -d '{"enabled":true,"scope":"security","interval_hours":6}'  # push a policy
```

## Layout

```
cmd/linux-dash-agent   entrypoint: run | check | version
cmd/devserver          reference control plane (in-memory)
internal/protocol      wire types shared with the control plane
internal/agent         report loop, enrolment, command dispatch
internal/metrics       /proc + statfs collectors
internal/apps          Docker (Engine API), PM2 (jlist), systemd
internal/journal       journald error reader
internal/packages      apt, dnf/yum, apk, pacman
internal/updater       policy scheduling, pre-flight, apply, reboot
internal/transport     HTTPS client (gzip, auth, TLS options)
internal/config        YAML config + validation
internal/state         persisted credentials and cursors
deploy/                systemd unit, OpenRC script, installer, example config
```

## Not yet covered

- zypper (openSUSE) isn't supported.
- Self-update of the agent binary isn't implemented.
- Roll-outs across many hosts are the dashboard's job (see PROTOCOL.md).
