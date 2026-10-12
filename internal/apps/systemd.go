package apps

import (
	"bufio"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/protocol"
)

// Systemd reports watched units plus any failed unit on the host.
type Systemd struct {
	Watch      []string
	ControlAny bool // allow start/stop/restart of units not in Watch
	Runner     execx.Runner
}

func (s *Systemd) Collect(ctx context.Context) *protocol.SystemdStatus {
	st := &protocol.SystemdStatus{Units: []protocol.SystemdUnit{}}
	seen := map[string]bool{}

	if len(s.Watch) > 0 {
		args := []string{"show", "--no-pager", "--property=Id,Description,ActiveState,SubState,UnitFileState,MainPID,NRestarts", "--"}
		res, err := s.Runner.Run(ctx, execx.Cmd{Name: "systemctl", Args: append(args, s.Watch...), Timeout: 20 * time.Second})
		if err != nil {
			st.Error = err.Error()
		} else {
			for _, u := range ParseSystemctlShow(string(res.Stdout)) {
				u.Watched = true
				seen[u.Name] = true
				st.Units = append(st.Units, u)
			}
		}
	}

	res, err := s.Runner.Run(ctx, execx.Cmd{
		Name: "systemctl", Args: []string{"list-units", "--state=failed", "--no-legend", "--plain", "--no-pager"}, Timeout: 20 * time.Second,
	})
	if err != nil {
		if st.Error == "" {
			st.Error = err.Error()
		}
		return st
	}
	for _, u := range ParseFailedUnits(string(res.Stdout)) {
		if !seen[u.Name] {
			st.Units = append(st.Units, u)
		}
	}
	return st
}

func (s *Systemd) Control(ctx context.Context, action, unit string) error {
	switch action {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("unsupported systemd action %q", action)
	}
	if !validUnit(unit) {
		return fmt.Errorf("invalid unit name %q", unit)
	}
	if !s.ControlAny && !slices.Contains(s.Watch, unit) {
		return fmt.Errorf("unit %q is not in systemd.watch and systemd.control_any is false", unit)
	}
	res, err := s.Runner.Run(ctx, execx.Cmd{Name: "systemctl", Args: []string{action, "--no-block", "--", unit}, Timeout: 60 * time.Second})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("systemctl %s %s exited %d: %s", action, unit, res.ExitCode, execx.Tail(res.Combined(), 500))
	}
	return nil
}

// ParseSystemctlShow parses blank-line separated property blocks.
func ParseSystemctlShow(out string) []protocol.SystemdUnit {
	var units []protocol.SystemdUnit
	cur := map[string]string{}
	flush := func() {
		if cur["Id"] != "" {
			pid, _ := strconv.Atoi(cur["MainPID"])
			restarts, _ := strconv.Atoi(cur["NRestarts"])
			units = append(units, protocol.SystemdUnit{
				Name: cur["Id"], Description: cur["Description"], ActiveState: cur["ActiveState"],
				SubState: cur["SubState"], UnitFileState: cur["UnitFileState"], MainPID: pid, Restarts: restarts,
			})
		}
		cur = map[string]string{}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cur[k] = v
		}
	}
	flush()
	return units
}

// ParseFailedUnits parses `systemctl list-units --state=failed --plain --no-legend`.
func ParseFailedUnits(out string) []protocol.SystemdUnit {
	var units []protocol.SystemdUnit
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "●"))
		if len(f) < 4 {
			continue
		}
		units = append(units, protocol.SystemdUnit{
			Name: f[0], ActiveState: f[2], SubState: f[3], Description: strings.Join(f[4:], " "),
		})
	}
	return units
}
