package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"linux-dash-agent/internal/protocol"
	"linux-dash-agent/internal/state"
)

// ErrRestart is returned by Run after the agent replaced its own binary; the
// service manager (systemd/OpenRC) starts the new version.
var ErrRestart = errors.New("restarting into the updated agent")

const (
	maxBinarySize = 200 << 20
	// A new version that restarts this many times without reporting in, or
	// fails this many reports in a row (~25 min with backoff), rolls back.
	maxUnconfirmed    = 5
	maxReportFailures = 10
)

var (
	versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	sha256Re  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// supervisor reports which service manager runs the agent, if any. Without
// one, a self-update could not restart the agent.
func supervisor() string {
	switch {
	case os.Getenv("INVOCATION_ID") != "":
		return "systemd"
	case os.Getenv("RC_SVCNAME") != "":
		return "openrc"
	}
	return ""
}

func (a *Agent) agentInfo() *protocol.AgentInfo {
	c := a.cfg
	sup := supervisor()
	return &protocol.AgentInfo{
		Version:               a.version,
		ReportIntervalSeconds: int(c.Intervals.Report.Seconds()),
		Actions: map[string]bool{
			"updates": c.Actions.Updates, "reboot": c.Actions.Reboot, "docker": c.Actions.Docker,
			"pm2": c.Actions.PM2, "systemd": c.Actions.Systemd, "agent_update": c.Actions.AgentUpdate,
		},
		Features: map[string]bool{
			"docker": a.docker != nil, "pm2": a.pm2 != nil, "systemd": a.systemd != nil,
			"journal": a.journal != nil, "updates": c.Updates.Enabled && a.updater.Manager != nil,
		},
		PackageManager: a.host.PackageManager,
		SelfUpdate:     c.Actions.AgentUpdate && sup != "",
		Supervisor:     sup,
		ConfigPath:     a.ConfigPath,
		LastUpdate:     a.store.Get().LastSelfUpdate,
	}
}

// selfUpdate downloads, verifies and installs a new agent binary, then asks
// to be restarted once the result has been reported.
func (a *Agent) selfUpdate(ctx context.Context, cmd protocol.Command, args protocol.AgentUpdateArgs) (string, error) {
	if !a.cfg.Actions.AgentUpdate {
		return "", fmt.Errorf("%w: actions.agent_update", errDisabled)
	}
	if supervisor() == "" {
		return "", errors.New("the agent isn't running under systemd or OpenRC, so it can't restart itself; re-run the installer instead")
	}
	if !versionRe.MatchString(args.Version) || !sha256Re.MatchString(args.SHA256) {
		return "", errors.New("invalid version or checksum in update request")
	}
	if want := "/downloads/linux-dash-agent-linux-" + runtime.GOARCH; args.Path != want {
		return "", fmt.Errorf("refusing download path %q (this agent needs %s)", args.Path, want)
	}
	if args.Version == a.version {
		return fmt.Sprintf("already running %s", a.version), nil
	}

	rep := a.newStream(protocol.CmdAgentUpdate, "command", cmd.ID, "")
	say := func(format string, v ...any) { rep.Line("agent", fmt.Sprintf(format, v...)) }
	fail := func(err error) (string, error) {
		say("failed: %v", err)
		rep.Phase("done")
		rep.Finish(protocol.UpdateRun{Status: "failed", Error: err.Error()})
		return "", err
	}

	exe, err := a.executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fail(fmt.Errorf("locating the agent binary: %w", err))
	}

	rep.Phase("downloading")
	say("Updating agent %s → %s (%s)", a.version, args.Version, exe)
	say("Downloading %s", args.Path)
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".linux-dash-agent-new-*")
	if err != nil {
		return fail(fmt.Errorf("creating temporary file next to %s: %w", exe, err))
	}
	defer os.Remove(tmp.Name()) // no-op once renamed into place
	h := sha256.New()
	n, err := a.client.Download(ctx, args.Path, io.MultiWriter(tmp, h), maxBinarySize)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(fmt.Errorf("download: %w", err))
	}
	say("Downloaded %.1f MB", float64(n)/(1<<20))

	rep.Phase("verifying")
	if got := hex.EncodeToString(h.Sum(nil)); got != args.SHA256 {
		return fail(fmt.Errorf("checksum mismatch: expected %s, got %s", args.SHA256, got))
	}
	say("SHA-256 matches %s…", args.SHA256[:16])
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return fail(err)
	}
	out, err := runFor(tmp.Name(), "version")
	if err != nil || out != args.Version {
		return fail(fmt.Errorf("new binary reports version %q, expected %q (%v)", out, args.Version, err))
	}
	say("New binary runs and reports version %s", out)
	if a.ConfigPath != "" {
		if out, err := runFor(tmp.Name(), "validate", "-config", a.ConfigPath); err != nil {
			return fail(fmt.Errorf("new version rejects the current config: %s", out))
		}
		say("New version accepts %s", a.ConfigPath)
	}

	rep.Phase("installing")
	backup := exe + ".previous"
	_ = os.Remove(backup)
	if err := os.Link(exe, backup); err != nil {
		if err := copyFile(exe, backup); err != nil {
			return fail(fmt.Errorf("keeping a copy of the current binary: %w", err))
		}
	}
	if err := a.store.Update(func(s *state.State) {
		s.PendingUpdate = &state.PendingUpdate{From: a.version, To: args.Version, Backup: backup, Binary: exe, At: time.Now().UTC()}
	}); err != nil {
		return fail(fmt.Errorf("saving update state: %w", err))
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		_ = a.store.Update(func(s *state.State) { s.PendingUpdate = nil })
		return fail(fmt.Errorf("installing the new binary: %w", err))
	}
	say("Installed; previous version kept at %s", backup)

	rep.Phase("restarting")
	say("Restarting via %s. The update is confirmed when %s reports in; it rolls back automatically if it can't.", supervisor(), args.Version)
	a.requestRestart(cmd.ID)
	rep.Finish(protocol.UpdateRun{Status: "success"})
	return fmt.Sprintf("installed %s; restarting", args.Version), nil
}

func runFor(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (a *Agent) requestRestart(commandID string) {
	a.mu.Lock()
	a.restartAfter = commandID
	a.mu.Unlock()
}

// restartDue reports whether the result that triggers a restart was delivered.
func (a *Agent) restartDue(rep *protocol.Report) bool {
	a.mu.Lock()
	id := a.restartAfter
	a.mu.Unlock()
	if id == "" {
		return false
	}
	for _, r := range rep.Results {
		if r.ID == id {
			return true
		}
	}
	return false
}

// checkPendingUpdate runs at startup. A new version that keeps restarting
// without ever reporting in is rolled back.
func (a *Agent) checkPendingUpdate() error {
	p := a.store.Get().PendingUpdate
	if p == nil {
		return nil
	}
	if a.version != p.To {
		// The old binary is running (restored by hand or by a rollback).
		a.finishPendingUpdate("rolled_back", fmt.Sprintf("agent started as %s instead of %s", a.version, p.To))
		return nil
	}
	p.Failures++
	_ = a.store.Update(func(s *state.State) { s.PendingUpdate = p })
	if p.Failures > maxUnconfirmed {
		return a.rollback(p, "the new version restarted repeatedly without reporting in")
	}
	return nil
}

// confirmUpdate runs after every successful report.
func (a *Agent) confirmUpdate() {
	p := a.store.Get().PendingUpdate
	if p == nil || a.version != p.To {
		return
	}
	_ = os.Remove(p.Backup)
	a.finishPendingUpdate("confirmed", "")
	a.log.Info("agent update confirmed", "from", p.From, "to", p.To)
}

// reportFailed runs after a failed report; a new version that can't reach
// the dashboard is rolled back.
func (a *Agent) reportFailed() error {
	p := a.store.Get().PendingUpdate
	if p == nil || a.version != p.To {
		return nil
	}
	p.ReportFailures++
	_ = a.store.Update(func(s *state.State) { s.PendingUpdate = p })
	if p.ReportFailures >= maxReportFailures {
		return a.rollback(p, "the new version couldn't report to the dashboard")
	}
	return nil
}

func (a *Agent) rollback(p *state.PendingUpdate, why string) error {
	a.log.Error("rolling back agent update", "from", p.To, "to", p.From, "reason", why)
	if err := os.Rename(p.Backup, p.Binary); err != nil {
		a.log.Error("rollback failed; reinstall the agent", "err", err)
		a.finishPendingUpdate("rolled_back", why+"; restoring the previous binary failed: "+err.Error())
		return nil
	}
	a.finishPendingUpdate("rolled_back", why)
	return ErrRestart
}

func (a *Agent) finishPendingUpdate(status, detail string) {
	_ = a.store.Update(func(s *state.State) {
		if s.PendingUpdate != nil {
			s.LastSelfUpdate = &protocol.SelfUpdateOutcome{From: s.PendingUpdate.From, To: s.PendingUpdate.To, Status: status, Detail: detail, FinishedAt: time.Now().UTC()}
		}
		s.PendingUpdate = nil
	})
}
