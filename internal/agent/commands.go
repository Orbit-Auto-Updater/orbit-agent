package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"orbit-agent/internal/execx"
	"orbit-agent/internal/packages"
	"orbit-agent/internal/protocol"
)

var errDisabled = errors.New("action disabled in agent config")

func (a *Agent) execute(ctx context.Context, cmd protocol.Command) {
	a.log.Info("command received", "id", cmd.ID, "type", cmd.Type)
	res := protocol.CommandResult{ID: cmd.ID, Type: cmd.Type, StartedAt: time.Now().UTC()}
	out, err := a.dispatch(ctx, cmd)
	res.FinishedAt, res.Output, res.OK = time.Now().UTC(), execx.Tail(out, 8000), err == nil
	if err != nil {
		res.Error = err.Error()
		a.log.Warn("command failed", "id", cmd.ID, "type", cmd.Type, "err", err)
	} else {
		a.log.Info("command completed", "id", cmd.ID, "type", cmd.Type, "took", res.FinishedAt.Sub(res.StartedAt).Round(time.Millisecond))
	}
	a.addResult(res)
}

// dispatch executes one allowlisted command. Every branch checks the local
// actions.* switch first so the control plane cannot exceed local policy.
func (a *Agent) dispatch(ctx context.Context, cmd protocol.Command) (string, error) {
	kind, action, _ := strings.Cut(cmd.Type, ".")
	switch kind {
	case "agent":
		if action == "update" {
			var args protocol.AgentUpdateArgs
			if err := decodeArgs(cmd.Args, &args); err != nil {
				return "", err
			}
			return a.selfUpdate(ctx, cmd, args)
		}
		if action != "refresh" {
			break
		}
		a.poke(a.appsKick)
		a.mu.Lock()
		a.updatesDirty = true
		a.mu.Unlock()
		return "collection triggered", nil

	case "updates":
		if !a.cfg.Updates.Enabled {
			return "", fmt.Errorf("%w: updates.enabled", errDisabled)
		}
		switch action {
		case "check":
			if err := a.updater.Check(ctx); err != nil {
				return "", err
			}
			s := a.updater.Status()
			sec := 0
			for _, p := range s.Packages {
				if p.Security {
					sec++
				}
			}
			return fmt.Sprintf("%d updates available (%d security)", len(s.Packages), sec), nil
		case "apply":
			if !a.cfg.Actions.Updates {
				return "", fmt.Errorf("%w: actions.updates", errDisabled)
			}
			var args protocol.UpdatesApplyArgs
			if err := decodeArgs(cmd.Args, &args); err != nil {
				return "", err
			}
			run := a.updater.Apply(ctx, "command", packages.UpgradeOptions{Packages: args.Packages, SecurityOnly: args.SecurityOnly}, false, cmd.ID)
			if run.Status != "success" {
				if run.Error == "" {
					run.Error = run.Status
				}
				return run.OutputTail, errors.New(run.Error)
			}
			return fmt.Sprintf("upgraded %d package(s)\n%s", len(run.Packages), run.OutputTail), nil
		}

	case "host":
		if action != "reboot" {
			break
		}
		if !a.cfg.Actions.Reboot {
			return "", fmt.Errorf("%w: actions.reboot", errDisabled)
		}
		var args protocol.RebootArgs
		if err := decodeArgs(cmd.Args, &args); err != nil {
			return "", err
		}
		if err := a.updater.Reboot(ctx, max(args.DelayMinutes, 1)); err != nil {
			return "", err
		}
		return fmt.Sprintf("reboot scheduled in %d minute(s)", max(args.DelayMinutes, 1)), nil

	case "docker":
		if !a.cfg.Actions.Docker {
			return "", fmt.Errorf("%w: actions.docker", errDisabled)
		}
		if a.docker == nil {
			return "", errors.New("docker is not available on this host")
		}
		var args protocol.ContainerArgs
		if err := decodeArgs(cmd.Args, &args); err != nil {
			return "", err
		}
		if err := a.docker.Control(ctx, action, args.ID); err != nil {
			return "", err
		}
		a.poke(a.appsKick)
		return fmt.Sprintf("container %s: %s ok", args.ID, action), nil

	case "pm2":
		if !a.cfg.Actions.PM2 {
			return "", fmt.Errorf("%w: actions.pm2", errDisabled)
		}
		if a.pm2 == nil {
			return "", errors.New("pm2 is not configured on this host")
		}
		var args protocol.PM2Args
		if err := decodeArgs(cmd.Args, &args); err != nil {
			return "", err
		}
		if err := a.pm2.Control(ctx, action, args.User, args.Name); err != nil {
			return "", err
		}
		a.poke(a.appsKick)
		return fmt.Sprintf("pm2 %s %s ok", action, args.Name), nil

	case "systemd":
		if !a.cfg.Actions.Systemd {
			return "", fmt.Errorf("%w: actions.systemd", errDisabled)
		}
		if a.systemd == nil {
			return "", errors.New("systemd is not available on this host")
		}
		var args protocol.UnitArgs
		if err := decodeArgs(cmd.Args, &args); err != nil {
			return "", err
		}
		if err := a.systemd.Control(ctx, action, args.Unit); err != nil {
			return "", err
		}
		a.poke(a.appsKick)
		return fmt.Sprintf("systemctl %s %s queued", action, args.Unit), nil
	}
	return "", fmt.Errorf("unknown command type %q", cmd.Type)
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid command args: %w", err)
	}
	return nil
}
