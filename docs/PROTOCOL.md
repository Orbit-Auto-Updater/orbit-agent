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

## 3. Live progress: `POST /api/agent/v1/progress`

Sent about once a second while an update run executes, whether started by an `updates.apply` command or the agent's own policy schedule. It's authenticated like reports.

```json
{
  "run_id": "run_3f9a…", "command_id": "cmd_123", "kind": "updates.apply", "trigger": "command",
  "started_at": "…", "phase": "installing", "total": 4,
  "seq": 12, "lines": [ { "t": "…", "s": "stdout", "text": "Setting up openssl (3.0.13-0ubuntu3.4) ..." } ],
  "installed": [ { "name": "openssl", "version": "3.0.13-0ubuntu3.4", "t": "…" } ],
  "done": false
}
```

- **`lines`:** the package manager's stdout/stderr plus `agent` lines summarising each step. Line *n* in a chunk has sequence number `seq + n`, so a re-sent chunk is idempotent. Store by `(run, seq)`.
- **`phase`:** one of `preflight`, `refreshing`, `installing`, `verifying`, `rebooting`, `done`.
- **`installed`:** packages finished since the previous chunk. They're parsed from apt (`Setting up …`), dnf/yum (`Upgrading : …`), apk (`(1/3) Upgrading …`) and pacman output.
- **`command_id`:** empty for scheduled runs. The control plane then creates a job for the run itself.
- **The final chunk:** has `done: true` with `status` (`success`, `failed` or `skipped`) and `error`.

If the dashboard is unreachable, the agent keeps up to 5,000 unsent lines and retries. If it answers 404 (an older dashboard), the agent stops streaming; the command result still carries the output tail.

## Commands

Unknown types, unknown argument fields, and actions disabled in the agent's `actions:` config are rejected with `ok: false`.

| Type                                    | Args                                         | Local switch      |
| --------------------------------------- | -------------------------------------------- | ----------------- |
| `agent.refresh`                         | none                                         | none              |
| `agent.update`                          | `{ "version": "0.3.0", "path": "/downloads/linux-dash-agent-linux-amd64", "sha256": "…" }` | `actions.agent_update` (needs systemd/OpenRC) |
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

### Self-update (`agent.update`)

The agent:
1. downloads `path` from the control plane and checks it against `sha256`;
2. runs the new binary's `version` (which must equal `version`) and `validate -config`;
3. keeps the current binary as `<binary>.previous`, renames the new one into place, and records a pending update;
4. exits after the result has been reported, so the service manager starts the new version.

The new version removes the backup after its first successful report and reports `agent.last_update.status = "confirmed"`. It rolls back to `.previous` (`status = "rolled_back"`) if it restarts more than 5 times without reporting, or fails 10 reports in a row.

The control plane should treat the job as done only when `agent_version` equals the target version.

Every report also carries `agent` (`AgentInfo`): the version, the report interval, the local `actions` allowlist, enabled `features`, the service manager, whether self-update is possible, and the last self-update outcome.

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
| Agents page                  | issues enrolment tokens; serves `install.sh` and `/downloads/linux-dash-agent-linux-<arch>` |
