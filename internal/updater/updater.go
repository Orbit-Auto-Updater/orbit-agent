// Package updater checks for and applies package updates, either on demand or
// on the schedule described by the active Policy.
package updater

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"orbit-agent/internal/execx"
	"orbit-agent/internal/packages"
	"orbit-agent/internal/protocol"
	"orbit-agent/internal/state"
)

// Reporter receives live progress of an update run (see agent.jobStream).
type Reporter interface {
	Phase(phase string)
	// Line records an output line; stream is stdout, stderr or agent.
	Line(stream, text string)
	Total(n int)
	Finish(run protocol.UpdateRun)
}

type nopReporter struct{}

func (nopReporter) Phase(string)              {}
func (nopReporter) Line(string, string)       {}
func (nopReporter) Total(int)                 {}
func (nopReporter) Finish(protocol.UpdateRun) {}

type Updater struct {
	// NewReporter, if set, creates a live progress stream for each run.
	NewReporter func(trigger, commandID, manager string) Reporter

	Manager     packages.Manager // nil if the host has none we support
	Runner      execx.Runner
	Store       *state.Store
	LocalPolicy protocol.Policy
	AllowRemote bool
	AllowReboot bool
	MinFreePct  float64
	FreePercent func(path string) (float64, error)
	Log         *slog.Logger
	OnChange    func() // called when Status changes

	op     sync.Mutex // serialises check/apply
	mu     sync.Mutex // guards status
	status protocol.UpdateStatus
}

var ErrBusy = errors.New("another package operation is already running")

func (u *Updater) Status() protocol.UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.Packages = append([]protocol.Package(nil), s.Packages...)
	if s.Manager == "" && u.Manager != nil {
		s.Manager = u.Manager.Name()
	}
	if s.LastRun == nil {
		s.LastRun = u.Store.Get().LastUpdateRun
	}
	return s
}

func (u *Updater) setStatus(fn func(*protocol.UpdateStatus)) {
	u.mu.Lock()
	fn(&u.status)
	u.mu.Unlock()
	if u.OnChange != nil {
		u.OnChange()
	}
}

// Policy returns the remote policy when allowed and present, else the local one.
func (u *Updater) Policy() protocol.Policy {
	if u.AllowRemote {
		if p := u.Store.Get().RemotePolicy; p != nil {
			return *p
		}
	}
	return u.LocalPolicy
}

// Check refreshes metadata and lists pending updates.
func (u *Updater) Check(ctx context.Context) error {
	if !u.op.TryLock() {
		return ErrBusy
	}
	defer u.op.Unlock()
	_, err := u.check(ctx, true)
	return err
}

func (u *Updater) check(ctx context.Context, refresh bool) ([]protocol.Package, error) {
	if u.Manager == nil {
		err := packages.ErrNoManager
		u.setStatus(func(s *protocol.UpdateStatus) { s.Error, s.CheckedAt = err.Error(), time.Now().UTC() })
		return nil, err
	}
	var err error
	if refresh {
		err = u.Manager.Refresh(ctx)
	}
	var pkgs []protocol.Package
	if err == nil {
		pkgs, err = u.Manager.List(ctx)
	}
	reboot := u.Manager.RebootRequired(ctx)
	u.setStatus(func(s *protocol.UpdateStatus) {
		s.Manager = u.Manager.Name()
		s.CheckedAt = time.Now().UTC()
		s.RebootRequired = reboot
		s.Error = ""
		if err != nil {
			s.Error = err.Error()
			return
		}
		s.Packages = pkgs
		if s.Packages == nil {
			s.Packages = []protocol.Package{}
		}
	})
	return pkgs, err
}

// Apply upgrades packages. trigger is "schedule" or "command"; commandID is
// the dashboard command that requested it (empty for scheduled runs).
func (u *Updater) Apply(ctx context.Context, trigger string, opts packages.UpgradeOptions, autoReboot bool, commandID string) protocol.UpdateRun {
	run := protocol.UpdateRun{Trigger: trigger, StartedAt: time.Now().UTC()}
	var rep Reporter = nopReporter{}
	if u.NewReporter != nil {
		mgr := ""
		if u.Manager != nil {
			mgr = u.Manager.Name()
		}
		rep = u.NewReporter(trigger, commandID, mgr)
	}
	say := func(format string, a ...any) { rep.Line("agent", fmt.Sprintf(format, a...)) }
	finish := func(status string, err error) protocol.UpdateRun {
		run.Status, run.FinishedAt = status, time.Now().UTC()
		if err != nil {
			run.Error = err.Error()
			say("%s: %s", status, run.Error)
		} else {
			say("Finished: %d package(s) in %s", len(run.Packages), run.FinishedAt.Sub(run.StartedAt).Round(time.Second))
		}
		r := run
		_ = u.Store.Update(func(s *state.State) { s.LastUpdateRun = &r })
		u.setStatus(func(s *protocol.UpdateStatus) { s.Running, s.LastRun = false, &r })
		u.Log.Info("update run finished", "trigger", trigger, "status", status, "packages", len(run.Packages), "error", run.Error)
		rep.Phase("done")
		rep.Finish(r)
		return run
	}

	if !u.op.TryLock() {
		// Another run owns the status; report this one without touching it.
		run.Status, run.Error, run.FinishedAt = "skipped", ErrBusy.Error(), time.Now().UTC()
		say("skipped: %s", ErrBusy)
		rep.Phase("done")
		rep.Finish(run)
		return run
	}
	defer u.op.Unlock()
	u.setStatus(func(s *protocol.UpdateStatus) { s.Running = true })
	// Every command run below streams its output to the reporter.
	ctx = execx.WithLineSink(ctx, rep.Line)

	if u.Manager == nil {
		return finish("failed", packages.ErrNoManager)
	}
	rep.Phase("preflight")
	say("Pre-flight checks (minimum free disk %.0f%%)", u.MinFreePct)
	if err := u.preflight(); err != nil {
		return finish("skipped", fmt.Errorf("pre-flight check failed: %w", err))
	}
	rep.Phase("refreshing")
	say("Refreshing %s package lists", u.Manager.Name())
	pending, err := u.check(ctx, true)
	if err != nil {
		return finish("failed", err)
	}
	targets, full, err := packages.Select(pending, opts)
	if err != nil {
		return finish("failed", err)
	}
	if full {
		targets = nil
		for _, p := range pending {
			targets = append(targets, p.Name)
		}
	}
	rep.Total(len(targets))
	if len(targets) == 0 {
		say("Nothing to install")
	} else {
		say("Installing %d package(s): %s", len(targets), strings.Join(targets, ", "))
	}
	rep.Phase("installing")
	applied, out, err := packages.Apply(ctx, u.Manager, pending, opts)
	run.Packages, run.OutputTail = applied, execx.Tail(out, 4000)
	if err != nil {
		rep.Phase("verifying")
		u.check(ctx, false) // refresh status after a partial run
		return finish("failed", err)
	}
	rep.Phase("verifying")
	say("Checking remaining updates and whether a reboot is needed")
	u.check(ctx, false)

	if autoReboot && u.Status().RebootRequired {
		if !u.AllowReboot {
			u.Log.Warn("reboot required but actions.reboot is disabled in agent config")
			say("A reboot is required, but reboots are disabled in the agent config")
		} else if err := u.Reboot(ctx, 1); err != nil {
			u.Log.Error("scheduling reboot failed", "err", err)
			say("Scheduling a reboot failed: %v", err)
		} else {
			run.RebootScheduled = true
			rep.Phase("rebooting")
			say("Reboot scheduled in 1 minute")
		}
	} else if u.Status().RebootRequired {
		say("A reboot is required to finish applying these updates")
	}
	return finish("success", nil)
}

func (u *Updater) preflight() error {
	if u.FreePercent == nil || u.MinFreePct <= 0 {
		return nil
	}
	for _, p := range []string{"/", "/var", "/boot"} {
		free, err := u.FreePercent(p)
		if err != nil {
			continue // path may not exist as a separate mount
		}
		if free < u.MinFreePct {
			return fmt.Errorf("only %.1f%% free on %s (minimum %.0f%%)", free, p, u.MinFreePct)
		}
	}
	return nil
}

// Reboot schedules a reboot in delay minutes via shutdown(8), leaving time
// for the result to be reported.
func (u *Updater) Reboot(ctx context.Context, delayMinutes int) error {
	if !u.AllowReboot {
		return errors.New("reboot is disabled by actions.reboot in the agent config")
	}
	if delayMinutes < 1 {
		delayMinutes = 1
	}
	res, err := u.Runner.Run(ctx, execx.Cmd{Name: "shutdown", Args: []string{"-r", "+" + strconv.Itoa(delayMinutes), "orbit-agent: scheduled reboot"}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("shutdown exited %d: %s", res.ExitCode, res.Combined())
	}
	return nil
}

// Tick runs a scheduled update if the active policy says one is due.
func (u *Updater) Tick(ctx context.Context, now time.Time) {
	p := u.Policy()
	st := u.Store.Get()
	var lastRun time.Time
	if st.LastUpdateRun != nil && st.LastUpdateRun.Trigger == "schedule" {
		lastRun = st.LastUpdateRun.StartedAt
	}
	windowStart, due := Due(p, now, st.LastUpdateWindow, lastRun)
	if !due {
		return
	}
	// Record the window first so a crash mid-run doesn't retrigger it.
	if err := u.Store.Update(func(s *state.State) { s.LastUpdateWindow = windowStart }); err != nil {
		u.Log.Error("saving update window", "err", err)
		return
	}
	u.Log.Info("starting scheduled update", "scope", p.Scope, "window", windowStart)
	u.Apply(ctx, "schedule", packages.UpgradeOptions{SecurityOnly: p.Scope == "security", Exclude: p.Exclude}, p.AutoReboot, "")
}
