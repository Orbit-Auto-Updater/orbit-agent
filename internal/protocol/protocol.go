// Package protocol defines the JSON messages exchanged between the agent and
// the Orbit control plane. The agent always initiates: it enrolls once,
// then POSTs a Report on every interval and receives queued Commands and the
// current update Policy in the response. See docs/PROTOCOL.md.
package protocol

import (
	"encoding/json"
	"time"
)

// Version is the protocol version, sent in the X-Agent-Protocol header.
const Version = "1"

const (
	PathEnroll   = "/api/agent/v1/enroll"
	PathReport   = "/api/agent/v1/report"
	PathProgress = "/api/agent/v1/progress"
)

// ---------------------------------------------------------------- enrolment

type EnrollRequest struct {
	Token        string   `json:"token"`
	AgentVersion string   `json:"agent_version"`
	Host         HostInfo `json:"host"`
}

type EnrollResponse struct {
	AgentID string `json:"agent_id"`
	Secret  string `json:"secret"`
}

// ------------------------------------------------------------------- report

type Report struct {
	AgentVersion string          `json:"agent_version"`
	SentAt       time.Time       `json:"sent_at"`
	Host         HostInfo        `json:"host"`
	Metrics      *Metrics        `json:"metrics,omitempty"`
	Apps         *Apps           `json:"apps,omitempty"`
	Updates      *UpdateStatus   `json:"updates,omitempty"`
	Logs         []LogEntry      `json:"logs,omitempty"`
	Results      []CommandResult `json:"results,omitempty"`
	// Agent describes the agent's own configuration (what it allows and runs).
	Agent *AgentInfo `json:"agent,omitempty"`
}

// AgentInfo is the agent's local configuration as it applies it, so the
// dashboard can show it and avoid offering actions the agent will refuse.
type AgentInfo struct {
	Version               string          `json:"version"`
	ReportIntervalSeconds int             `json:"report_interval_seconds"`
	Actions               map[string]bool `json:"actions"`  // updates, reboot, docker, pm2, systemd, agent_update
	Features              map[string]bool `json:"features"` // docker, pm2, systemd, journal, updates
	PackageManager        string          `json:"package_manager,omitempty"`
	// SelfUpdate is true when agent.update can work (allowed, and running
	// under a service manager that restarts it).
	SelfUpdate bool   `json:"self_update"`
	Supervisor string `json:"supervisor,omitempty"` // systemd, openrc, or empty
	ConfigPath string `json:"config_path,omitempty"`
	// LastUpdate reports the outcome of the most recent self-update.
	LastUpdate *SelfUpdateOutcome `json:"last_update,omitempty"`
}

type SelfUpdateOutcome struct {
	From       string    `json:"from"`
	To         string    `json:"to"`
	Status     string    `json:"status"` // confirmed, rolled_back
	Detail     string    `json:"detail,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

type ReportResponse struct {
	Commands []Command `json:"commands,omitempty"`
	// Policy, when present, replaces the agent's auto-update policy.
	Policy *Policy `json:"policy,omitempty"`
}

type HostInfo struct {
	Hostname       string    `json:"hostname"`
	OSName         string    `json:"os_name"`
	OSID           string    `json:"os_id"`
	OSVersion      string    `json:"os_version"`
	Kernel         string    `json:"kernel"`
	Arch           string    `json:"arch"`
	CPUCores       int       `json:"cpu_cores"`
	MemTotalBytes  uint64    `json:"mem_total_bytes"`
	BootTime       time.Time `json:"boot_time"`
	IPs            []string  `json:"ips,omitempty"`
	PackageManager string    `json:"package_manager,omitempty"`
}

// -------------------------------------------------------------------- metrics

type Metrics struct {
	CollectedAt      time.Time  `json:"collected_at"`
	CPUPercent       float64    `json:"cpu_percent"`
	CPUIOWaitPercent float64    `json:"cpu_iowait_percent"`
	Load1            float64    `json:"load1"`
	Load5            float64    `json:"load5"`
	Load15           float64    `json:"load15"`
	MemTotalBytes    uint64     `json:"mem_total_bytes"`
	MemUsedBytes     uint64     `json:"mem_used_bytes"`
	MemAvailBytes    uint64     `json:"mem_available_bytes"`
	MemPercent       float64    `json:"mem_percent"`
	SwapTotalBytes   uint64     `json:"swap_total_bytes"`
	SwapUsedBytes    uint64     `json:"swap_used_bytes"`
	UptimeSeconds    float64    `json:"uptime_seconds"`
	Disks            []Disk     `json:"disks"`
	Net              []NetIface `json:"net"`
	Errors           []string   `json:"errors,omitempty"`
}

type Disk struct {
	Mount         string  `json:"mount"`
	Device        string  `json:"device"`
	FSType        string  `json:"fs_type"`
	TotalBytes    uint64  `json:"total_bytes"`
	UsedBytes     uint64  `json:"used_bytes"`
	FreeBytes     uint64  `json:"free_bytes"`
	UsedPercent   float64 `json:"used_percent"`
	InodesPercent float64 `json:"inodes_used_percent"`
}

type NetIface struct {
	Name          string  `json:"name"`
	RxBytesPerSec float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec float64 `json:"tx_bytes_per_sec"`
	RxBytesTotal  uint64  `json:"rx_bytes_total"`
	TxBytesTotal  uint64  `json:"tx_bytes_total"`
	RxErrors      uint64  `json:"rx_errors"`
	TxErrors      uint64  `json:"tx_errors"`
}

// ----------------------------------------------------------------------- apps

type Apps struct {
	CollectedAt time.Time      `json:"collected_at"`
	Docker      *DockerStatus  `json:"docker,omitempty"`
	PM2         *PM2Status     `json:"pm2,omitempty"`
	Systemd     *SystemdStatus `json:"systemd,omitempty"`
}

type DockerStatus struct {
	Error      string      `json:"error,omitempty"`
	Containers []Container `json:"containers"`
}

type Container struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Image        string    `json:"image"`
	State        string    `json:"state"` // running, exited, restarting, paused, created, dead
	Status       string    `json:"status"`
	Health       string    `json:"health,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	RestartCount int       `json:"restart_count"`
	ExitCode     int       `json:"exit_code"`
	OOMKilled    bool      `json:"oom_killed,omitempty"`
	CPUPercent   float64   `json:"cpu_percent"`
	MemBytes     uint64    `json:"mem_bytes"`
	MemLimit     uint64    `json:"mem_limit_bytes"`
	Ports        []string  `json:"ports,omitempty"`
}

type PM2Status struct {
	Errors    []string     `json:"errors,omitempty"`
	Processes []PM2Process `json:"processes"`
}

type PM2Process struct {
	User      string    `json:"user"`
	ID        int       `json:"pm_id"`
	Name      string    `json:"name"`
	PID       int       `json:"pid"`
	Status    string    `json:"status"` // online, stopped, errored, launching, stopping
	Mode      string    `json:"mode"`   // cluster, fork
	CPU       float64   `json:"cpu_percent"`
	MemBytes  uint64    `json:"mem_bytes"`
	Restarts  int       `json:"restarts"`
	Unstable  int       `json:"unstable_restarts"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

type SystemdStatus struct {
	Error string        `json:"error,omitempty"`
	Units []SystemdUnit `json:"units"`
}

type SystemdUnit struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	UnitFileState string `json:"unit_file_state,omitempty"`
	MainPID       int    `json:"main_pid,omitempty"`
	Restarts      int    `json:"restarts,omitempty"`
	Watched       bool   `json:"watched"`
}

// --------------------------------------------------------------------- logs

type LogEntry struct {
	Time     time.Time `json:"time"`
	Priority int       `json:"priority"` // syslog: 0 emerg … 3 err, 4 warning
	Unit     string    `json:"unit,omitempty"`
	Message  string    `json:"message"`
}

// ------------------------------------------------------------------ updates

type UpdateStatus struct {
	Manager        string     `json:"manager"`
	CheckedAt      time.Time  `json:"checked_at"`
	Error          string     `json:"error,omitempty"`
	Packages       []Package  `json:"packages"`
	RebootRequired bool       `json:"reboot_required"`
	Running        bool       `json:"running"`
	LastRun        *UpdateRun `json:"last_run,omitempty"`
}

type Package struct {
	Name      string `json:"name"`
	Arch      string `json:"arch,omitempty"`
	Current   string `json:"current,omitempty"`
	Available string `json:"available"`
	Repo      string `json:"repo,omitempty"`
	Security  bool   `json:"security"`
	Advisory  string `json:"advisory,omitempty"`
	// Source is the source package name (Debian source, SRPM name, apk
	// origin). Vulnerability databases such as OSV index by it.
	Source string `json:"source,omitempty"`
}

type UpdateRun struct {
	Trigger         string    `json:"trigger"` // schedule, command
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	Status          string    `json:"status"` // success, failed, skipped
	Packages        []string  `json:"packages,omitempty"`
	Error           string    `json:"error,omitempty"`
	RebootScheduled bool      `json:"reboot_scheduled,omitempty"`
	OutputTail      string    `json:"output_tail,omitempty"`
}

// Policy controls unattended updates. It is set in the agent config and may be
// replaced by the control plane via ReportResponse.Policy.
type Policy struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Scope is "security" or "all".
	Scope string `json:"scope" yaml:"scope"`
	// Window runs updates at most once per maintenance window…
	Window *Window `json:"window,omitempty" yaml:"window,omitempty"`
	// …or IntervalHours runs them every N hours. Window takes precedence.
	IntervalHours int      `json:"interval_hours,omitempty" yaml:"interval_hours,omitempty"`
	AutoReboot    bool     `json:"auto_reboot" yaml:"auto_reboot"`
	Exclude       []string `json:"exclude,omitempty" yaml:"exclude,omitempty"`
}

type Window struct {
	// Days such as ["sun"] or ["mon","wed"]; empty means every day.
	Days            []string `json:"days,omitempty" yaml:"days,omitempty"`
	Start           string   `json:"start" yaml:"start"` // "HH:MM"
	DurationMinutes int      `json:"duration_minutes" yaml:"duration_minutes"`
	Timezone        string   `json:"timezone,omitempty" yaml:"timezone,omitempty"`
}

// ----------------------------------------------------------------- progress

// Progress streams a long-running job (an update run) while it executes. The
// agent POSTs one roughly every second; lines carry consecutive sequence
// numbers starting at Seq, so re-sent chunks are idempotent.
type Progress struct {
	RunID     string    `json:"run_id"`
	CommandID string    `json:"command_id,omitempty"` // empty for scheduled runs
	Kind      string    `json:"kind"`                 // "updates.apply"
	Trigger   string    `json:"trigger"`              // "command" or "schedule"
	StartedAt time.Time `json:"started_at"`
	Phase     string    `json:"phase,omitempty"` // preflight, refreshing, installing, verifying, rebooting, done
	Total     int       `json:"total,omitempty"` // packages expected
	Seq       int       `json:"seq"`
	Lines     []LogLine `json:"lines,omitempty"`
	// Installed lists packages completed since the previous chunk.
	Installed []InstalledPackage `json:"installed,omitempty"`
	Done      bool               `json:"done,omitempty"`
	Status    string             `json:"status,omitempty"` // success, failed, skipped (when Done)
	Error     string             `json:"error,omitempty"`
}

type LogLine struct {
	Time   time.Time `json:"t"`
	Stream string    `json:"s"` // stdout, stderr, agent
	Text   string    `json:"text"`
}

type InstalledPackage struct {
	Name    string    `json:"name"`
	Version string    `json:"version,omitempty"`
	Time    time.Time `json:"t"`
}

// ----------------------------------------------------------------- commands

// Command types. Anything else is rejected by the agent.
const (
	CmdUpdatesCheck   = "updates.check"
	CmdUpdatesApply   = "updates.apply"
	CmdHostReboot     = "host.reboot"
	CmdDockerStart    = "docker.start"
	CmdDockerStop     = "docker.stop"
	CmdDockerRestart  = "docker.restart"
	CmdPM2Start       = "pm2.start"
	CmdPM2Stop        = "pm2.stop"
	CmdPM2Restart     = "pm2.restart"
	CmdSystemdStart   = "systemd.start"
	CmdSystemdStop    = "systemd.stop"
	CmdSystemdRestart = "systemd.restart"
	CmdAgentRefresh   = "agent.refresh"
	CmdAgentUpdate    = "agent.update"
)

type Command struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Args     json.RawMessage `json:"args,omitempty"`
	IssuedAt time.Time       `json:"issued_at,omitzero"`
}

type CommandResult struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	Output     string    `json:"output,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Argument payloads.

type UpdatesApplyArgs struct {
	Packages     []string `json:"packages,omitempty"`
	SecurityOnly bool     `json:"security_only,omitempty"`
}

type RebootArgs struct {
	DelayMinutes int `json:"delay_minutes,omitempty"`
}

type ContainerArgs struct {
	ID string `json:"id"` // container ID or name
}

type PM2Args struct {
	User string `json:"user,omitempty"`
	Name string `json:"name"`
}

// AgentUpdateArgs points the agent at a new release served by the dashboard.
type AgentUpdateArgs struct {
	Version string `json:"version"`
	Path    string `json:"path"`   // e.g. /downloads/orbit-agent-linux-amd64
	SHA256  string `json:"sha256"` // of that file
}

type UnitArgs struct {
	Unit string `json:"unit"`
}
