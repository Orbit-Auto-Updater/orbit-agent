# Agent ↔ control plane protocol (v1)

The dashboard ("control plane") implements two HTTPS endpoints. The agent always initiates connections, so servers need no inbound ports. `cmd/devserver` is a minimal reference implementation of the server side; the Linux Dash dashboard (`../linux-dash`, `app/api/agent/v1/*`) is the full one. The Go types live in [`internal/protocol`](../internal/protocol/protocol.go).

## Transport

- `POST` with a JSON body, gzip-compressed (`Content-Encoding: gzip`).
- Headers on every request: `User-Agent: linux-dash-agent/<version>` and `X-Agent-Protocol: 1`.
- Authenticated requests also send `X-Agent-ID: <agent_id>` and `Authorization: Bearer <secret>`.
- Return `401` or `403` for unknown or revoked agents. The agent logs an error and retries every 5 minutes.
- Any other non-2xx response, or a network error, makes the agent back off (exponential with jitter, capped at 5 min). It keeps queued logs and command results and resends them on the next successful report.

## 1. Enrol: `POST /api/agent/v1/enroll`

Sent once, when the agent has no stored credentials.

```json
{ "token": "<enroll token>", "agent_version": "0.1.0", "host": { "hostname": "web-prod-01", "os_name": "Ubuntu 24.04.1 LTS", "kernel": "6.8.0-45-generic", "arch": "amd64", "cpu_cores": 8, "mem_total_bytes": 16697266176, "package_manager": "apt", "ips": ["10.0.1.11"] } }
```

Response:

```json
{ "agent_id": "agt_…", "secret": "<random, ≥32 bytes>" }
```

- Store only a hash of the secret.
- Compare tokens and secrets in constant time.
- Enrolment tokens should be single-use or short-lived.

## 2. Report: `POST /api/agent/v1/report`

Sent every `intervals.report` (default 10s). It is also sent immediately after a command finishes. Optional sections are omitted when unchanged:

| Field      | When present                                                                 |
| ---------- | ---------------------------------------------------------------------------- |
| `host`     | always                                                                       |
| `metrics`  | always: CPU %, iowait, load, memory/swap, per-mount disk and inode %, per-interface byte rates and errors, uptime |
| `apps`     | after each apps collection (default 30s): Docker containers (state, health, restarts, OOM, CPU, memory), PM2 processes, watched and failed systemd units |
| `updates`  | when the update status changed, and at least every 5 min: pending packages (with `security`, `advisory`, and `source`, the source package name vulnerability databases use), `reboot_required`, `running`, `last_run` |
| `logs`     | new journald entries at or above `journal.priority` since the last report    |
| `results`  | results of commands finished since the last successful report                |

Treat a host as **offline** when no report arrives for about 3× its interval.

Response:

```json
{
  "commands": [ { "id": "cmd_123", "type": "docker.restart", "args": { "id": "api-gateway" } } ],
  "policy":   { "enabled": true, "scope": "security", "window": { "days": ["sun"], "start": "02:00", "duration_minutes": 120, "timezone": "UTC" }, "auto_reboot": false, "exclude": [] }
}
```

- **Commands:** return every pending command for the agent until a report contains a `results` entry with the same `id`, then drop it. The agent de-duplicates by `id` (for 1 hour), so re-delivery is safe.
- **Policy:** include it only when it changes. The agent persists it, validates it, and ignores it if `updates.allow_remote_policy` is false.

## Commands

Unknown types, unknown argument fields, and actions disabled in the agent's `actions:` config are rejected with `ok: false`.

| Type                                    | Args                                         | Local switch      |
| --------------------------------------- | -------------------------------------------- | ----------------- |
| `agent.refresh`                         | none                                         | none              |
| `updates.check`                         | none                                         | `updates.enabled` |
| `updates.apply`                         | `{ "packages"?: [..], "security_only"?: bool }` | `actions.updates` |
| `host.reboot`                           | `{ "delay_minutes"?: int }` (min 1)          | `actions.reboot` (off by default) |
| `docker.start` / `.stop` / `.restart`   | `{ "id": "<container id or name>" }`         | `actions.docker`  |
| `pm2.start` / `.stop` / `.restart`      | `{ "name": "<app>", "user"?: "<unix user>" }` | `actions.pm2`     |
| `systemd.start` / `.stop` / `.restart`  | `{ "unit": "nginx.service" }` (must be in `systemd.watch` unless `control_any`) | `actions.systemd` |

Result:

```json
{ "id": "cmd_123", "type": "docker.restart", "ok": true, "output": "container api-gateway: restart ok", "error": "", "started_at": "…", "finished_at": "…" }
```

There is no arbitrary shell execution, by design.

## Update orchestration

Each agent runs its own policy window, so updates still happen if the dashboard is unreachable. The dashboard should handle anything that spans hosts by sending `updates.apply` to hosts in sequence and waiting for each result, for example:

- canary roll-outs ("1 host → 25% → 100%")
- stopping a roll-out when a host fails
- requiring approval before reboots

## Mapping to the dashboard

| Dashboard view               | Source                                                          |
| ---------------------------- | --------------------------------------------------------------- |
| Overview KPIs and graphs     | `metrics` aggregated across agents; `updates.packages`; alerts derived from metrics, apps and logs |
| Servers cards and drawer     | `host`, `metrics`, `logs`                                       |
| Applications                 | `apps.docker`, `apps.pm2`, `apps.systemd`, and the `docker.*` / `pm2.*` / `systemd.*` commands |
| Updates                      | `updates`, `updates.last_run`, `policy`, `updates.apply`        |
| Alerts & logs                | `logs`, plus threshold rules evaluated on the server            |
| Settings → Agent enrolment   | issues enrolment tokens; serves `install.sh` and `/downloads/linux-dash-agent-linux-<arch>` |
