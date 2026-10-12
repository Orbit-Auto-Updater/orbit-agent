package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/user"
	"strings"
	"time"

	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/protocol"
)

// PM2Instance is one PM2 daemon, identified by the Unix user that owns it.
type PM2Instance struct {
	User   string `yaml:"user"`
	Binary string `yaml:"binary"` // path to pm2; default "pm2"
	Home   string `yaml:"home"`   // PM2_HOME, if non-standard
}

type PM2 struct {
	Instances []PM2Instance
	Runner    execx.Runner
}

// command builds an invocation of pm2 as the instance's user. When the agent
// already runs as that user the binary is called directly; otherwise runuser
// switches to it (the agent runs as root).
func (p *PM2) command(inst PM2Instance, args ...string) (execx.Cmd, error) {
	bin := inst.Binary
	if bin == "" {
		bin = "pm2"
	}
	var env []string
	if inst.Home != "" {
		env = append(env, "PM2_HOME="+inst.Home)
	}
	cur, _ := user.Current()
	if inst.User == "" || (cur != nil && cur.Username == inst.User) {
		return execx.Cmd{Name: bin, Args: args, Env: env, Timeout: 30 * time.Second}, nil
	}
	if !validUser(inst.User) {
		return execx.Cmd{}, fmt.Errorf("invalid pm2 user %q", inst.User)
	}
	// runuser resets the environment, so pass PM2_HOME via env(1).
	full := []string{"-u", inst.User, "--", "env"}
	full = append(full, env...)
	full = append(full, bin)
	full = append(full, args...)
	return execx.Cmd{Name: "runuser", Args: full, Timeout: 30 * time.Second}, nil
}

func (p *PM2) Collect(ctx context.Context) *protocol.PM2Status {
	st := &protocol.PM2Status{Processes: []protocol.PM2Process{}}
	for _, inst := range p.Instances {
		cmd, err := p.command(inst, "jlist")
		if err != nil {
			st.Errors = append(st.Errors, err.Error())
			continue
		}
		res, err := p.Runner.Run(ctx, cmd)
		if err == nil && res.ExitCode != 0 {
			err = fmt.Errorf("pm2 jlist exited %d: %s", res.ExitCode, execx.Tail(res.Combined(), 300))
		}
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", label(inst), err))
			continue
		}
		procs, err := ParsePM2JList(res.Stdout, inst.User)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", label(inst), err))
			continue
		}
		st.Processes = append(st.Processes, procs...)
	}
	return st
}

func (p *PM2) Control(ctx context.Context, action, userName, name string) error {
	switch action {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("unsupported pm2 action %q", action)
	}
	if !validName(name) {
		return fmt.Errorf("invalid pm2 process name %q", name)
	}
	inst, ok := p.instance(userName)
	if !ok {
		return fmt.Errorf("no pm2 instance configured for user %q", userName)
	}
	cmd, err := p.command(inst, action, name)
	if err != nil {
		return err
	}
	res, err := p.Runner.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("pm2 %s %s exited %d: %s", action, name, res.ExitCode, execx.Tail(res.Combined(), 500))
	}
	return nil
}

func (p *PM2) instance(userName string) (PM2Instance, bool) {
	for _, in := range p.Instances {
		if in.User == userName || (userName == "" && len(p.Instances) == 1) {
			return in, true
		}
	}
	return PM2Instance{}, false
}

func label(inst PM2Instance) string {
	if inst.User == "" {
		return "pm2"
	}
	return "pm2@" + inst.User
}

type pm2Entry struct {
	Name  string `json:"name"`
	PID   int    `json:"pid"`
	PMID  int    `json:"pm_id"`
	Monit struct {
		Memory uint64  `json:"memory"`
		CPU    float64 `json:"cpu"`
	} `json:"monit"`
	Env struct {
		Status           string `json:"status"`
		ExecMode         string `json:"exec_mode"`
		RestartTime      int    `json:"restart_time"`
		UnstableRestarts int    `json:"unstable_restarts"`
		PMUptime         int64  `json:"pm_uptime"`
	} `json:"pm2_env"`
}

// ParsePM2JList parses `pm2 jlist`. PM2 sometimes prints banners or update
// notices before the JSON array, so parsing starts at the first '['.
func ParsePM2JList(out []byte, owner string) ([]protocol.PM2Process, error) {
	i := bytes.IndexByte(out, '[')
	if i < 0 {
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("pm2 jlist: no JSON array in output")
	}
	var entries []pm2Entry
	if err := json.NewDecoder(bytes.NewReader(out[i:])).Decode(&entries); err != nil {
		return nil, fmt.Errorf("pm2 jlist: %w", err)
	}
	procs := make([]protocol.PM2Process, 0, len(entries))
	for _, e := range entries {
		p := protocol.PM2Process{
			User: owner, ID: e.PMID, Name: e.Name, PID: e.PID, Status: e.Env.Status,
			Mode: strings.TrimSuffix(e.Env.ExecMode, "_mode"), CPU: e.Monit.CPU, MemBytes: e.Monit.Memory,
			Restarts: e.Env.RestartTime, Unstable: e.Env.UnstableRestarts,
		}
		if e.Env.PMUptime > 0 && e.Env.Status == "online" {
			p.StartedAt = time.UnixMilli(e.Env.PMUptime).UTC()
		}
		procs = append(procs, p)
	}
	return procs, nil
}
