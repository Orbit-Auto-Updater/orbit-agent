// Package agent wires collectors, the updater and the transport together and
// runs the report loop.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"linux-dash-agent/internal/apps"
	"linux-dash-agent/internal/config"
	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/journal"
	"linux-dash-agent/internal/metrics"
	"linux-dash-agent/internal/packages"
	"linux-dash-agent/internal/protocol"
	"linux-dash-agent/internal/state"
	"linux-dash-agent/internal/transport"
	"linux-dash-agent/internal/updater"
)

const (
	maxBackoff       = 5 * time.Minute
	maxQueuedLogs    = 500
	maxQueuedResults = 100
	updatesResync    = 5 * time.Minute // resend update status even if unchanged
)

type Agent struct {
	cfg     *config.Config
	log     *slog.Logger
	version string
	store   *state.Store
	client  *transport.Client
	runner  execx.Runner

	metrics *metrics.Collector
	docker  *apps.Docker
	pm2     *apps.PM2
	systemd *apps.Systemd
	journal *journal.Reader
	updater *updater.Updater
	host    protocol.HostInfo

	appsKick chan struct{}
	wake     chan struct{}

	mu            sync.Mutex
	apps          *protocol.Apps
	appsSent      bool
	updatesDirty  bool
	updatesSentAt time.Time
	logs          []protocol.LogEntry
	cursor        string
	results       []protocol.CommandResult
	seenCommands  map[string]time.Time
}

func New(cfg *config.Config, store *state.Store, log *slog.Logger, version string) (*Agent, error) {
	r := execx.OS{}
	a := &Agent{
		cfg: cfg, log: log, version: version, store: store, runner: r,
		metrics:      metrics.NewCollector(cfg.Metrics.ExcludeInterfaces, cfg.Metrics.ExcludeMounts),
		appsKick:     make(chan struct{}, 1),
		wake:         make(chan struct{}, 1),
		seenCommands: map[string]time.Time{},
		updatesDirty: true,
		cursor:       store.Get().JournalCursor,
	}

	if cfg.Docker.Enabled {
		if _, err := os.Stat(cfg.Docker.Socket); err == nil {
			a.docker = apps.NewDocker(cfg.Docker.Socket)
		} else {
			log.Info("docker socket not found; container monitoring disabled", "socket", cfg.Docker.Socket)
		}
	}
	if cfg.PM2.Enabled && len(cfg.PM2.Instances) > 0 {
		a.pm2 = &apps.PM2{Instances: cfg.PM2.Instances, Runner: r}
	}
	if cfg.Systemd.Enabled && has(r, "systemctl") {
		a.systemd = &apps.Systemd{Watch: cfg.Systemd.Watch, ControlAny: cfg.Systemd.ControlAny, Runner: r}
	}
	if cfg.Journal.Enabled && has(r, "journalctl") {
		a.journal = &journal.Reader{Priority: cfg.Journal.Priority, MaxEntries: cfg.Journal.MaxEntries, Runner: r}
	}

	mgr, err := packages.Detect(r)
	if err != nil {
		log.Warn("package updates unavailable", "err", err)
	}
	a.updater = &updater.Updater{
		Manager: mgr, Runner: r, Store: store,
		LocalPolicy: cfg.Updates.Policy, AllowRemote: cfg.Updates.AllowRemotePolicy,
		AllowReboot: cfg.Actions.Reboot, MinFreePct: cfg.Updates.MinFreeDiskPercent,
		FreePercent: metrics.FreePercent, Log: log,
		OnChange: func() {
			a.mu.Lock()
			a.updatesDirty = true
			a.mu.Unlock()
		},
	}

	a.host = a.metrics.HostInfo()
	if mgr != nil {
		a.host.PackageManager = mgr.Name()
	}

	client, err := transport.New(transport.Options{
		BaseURL: cfg.Server.URL, CAFile: cfg.Server.CAFile,
		InsecureSkipVerify: cfg.Server.InsecureSkipVerify, UserAgent: "linux-dash-agent/" + version,
	})
	if err != nil {
		return nil, err
	}
	a.client = client
	return a, nil
}

func has(r execx.Runner, bin string) bool { _, err := r.LookPath(bin); return err == nil }

// Run blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.cfg.Server.InsecureSkipVerify {
		a.log.Warn("TLS certificate verification is disabled (server.insecure_skip_verify)")
	}
	if err := a.ensureEnrolled(ctx); err != nil {
		return err
	}
	a.log.Info("agent started", "version", a.version, "server", a.cfg.Server.URL, "agent_id", a.store.Get().AgentID,
		"package_manager", a.host.PackageManager, "docker", a.docker != nil, "pm2", a.pm2 != nil, "systemd", a.systemd != nil)

	a.metrics.Collect() // prime CPU/network deltas
	go a.appsLoop(ctx)
	if a.cfg.Updates.Enabled {
		go a.updatesLoop(ctx)
	}
	a.reportLoop(ctx)
	return nil
}

// Snapshot collects one full report without contacting the control plane.
// Used by `linux-dash-agent check` to verify collection on a host.
func (a *Agent) Snapshot(ctx context.Context, withUpdates bool) *protocol.Report {
	a.metrics.Collect()
	time.Sleep(time.Second) // give CPU and network rates a sampling window
	a.collectApps(ctx)
	if withUpdates {
		if err := a.updater.Check(ctx); err != nil {
			a.log.Warn("update check failed", "err", err)
		}
	}
	return a.buildReport(ctx)
}

func (a *Agent) ensureEnrolled(ctx context.Context) error {
	st := a.store.Get()
	if st.AgentID != "" && st.Secret != "" && st.ServerURL == a.cfg.Server.URL {
		a.client.SetCredentials(st.AgentID, st.Secret)
		return nil
	}
	if a.cfg.Server.EnrollToken == "" {
		return errors.New("agent is not enrolled: set server.enroll_token in the config")
	}
	backoff := 5 * time.Second
	for {
		resp, err := a.client.Enroll(ctx, protocol.EnrollRequest{Token: a.cfg.Server.EnrollToken, AgentVersion: a.version, Host: a.host})
		if err == nil {
			if err := a.store.Update(func(s *state.State) {
				s.AgentID, s.Secret, s.ServerURL = resp.AgentID, resp.Secret, a.cfg.Server.URL
			}); err != nil {
				return err
			}
			a.client.SetCredentials(resp.AgentID, resp.Secret)
			a.log.Info("enrolled with control plane", "agent_id", resp.AgentID)
			return nil
		}
		a.log.Error("enrolment failed; retrying", "err", err, "in", backoff)
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = nextBackoff(backoff)
	}
}

func (a *Agent) reportLoop(ctx context.Context) {
	interval := a.cfg.Intervals.Report
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-a.wake:
		}

		rep := a.buildReport(ctx)
		resp, err := a.client.Report(ctx, rep)
		if err != nil {
			a.requeue(rep)
			if ctx.Err() != nil {
				return
			}
			if wait < interval {
				wait = interval
			}
			wait = nextBackoff(wait)
			level := slog.LevelWarn
			if errors.Is(err, transport.ErrUnauthorized) {
				level = slog.LevelError
				wait = maxBackoff
			}
			a.log.Log(ctx, level, "report failed", "err", err, "retry_in", wait.Round(time.Second))
			continue
		}
		a.commit(rep)
		a.handleResponse(ctx, resp)
		wait = jitter(interval)
	}
}

// buildReport snapshots everything that should go to the control plane now.
func (a *Agent) buildReport(ctx context.Context) *protocol.Report {
	rep := &protocol.Report{AgentVersion: a.version, SentAt: time.Now().UTC(), Host: a.host, Metrics: a.metrics.Collect()}

	if a.journal != nil {
		a.mu.Lock()
		cursor := a.cursor
		a.mu.Unlock()
		entries, next, err := a.journal.Read(ctx, cursor)
		if err != nil {
			a.log.Debug("journal read failed", "err", err)
		}
		a.mu.Lock()
		a.cursor = next
		a.logs = capTail(append(a.logs, entries...), maxQueuedLogs)
		a.mu.Unlock()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.apps != nil && !a.appsSent {
		rep.Apps = a.apps
	}
	if a.updatesDirty || time.Since(a.updatesSentAt) > updatesResync {
		s := a.updater.Status()
		rep.Updates = &s
	}
	rep.Logs, a.logs = a.logs, nil
	rep.Results, a.results = a.results, nil
	return rep
}

func (a *Agent) requeue(rep *protocol.Report) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = capTail(append(rep.Logs, a.logs...), maxQueuedLogs)
	a.results = capTail(append(rep.Results, a.results...), maxQueuedResults)
}

func (a *Agent) commit(rep *protocol.Report) {
	a.mu.Lock()
	if rep.Apps != nil && a.apps == rep.Apps {
		a.appsSent = true
	}
	if rep.Updates != nil {
		a.updatesDirty, a.updatesSentAt = false, time.Now()
	}
	cursor := a.cursor
	a.mu.Unlock()
	if cursor != a.store.Get().JournalCursor {
		if err := a.store.Update(func(s *state.State) { s.JournalCursor = cursor }); err != nil {
			a.log.Warn("saving journal cursor", "err", err)
		}
	}
}

func (a *Agent) handleResponse(ctx context.Context, resp protocol.ReportResponse) {
	if p := resp.Policy; p != nil && a.cfg.Updates.AllowRemotePolicy {
		if err := config.ValidatePolicy(*p); err != nil {
			a.log.Warn("ignoring invalid policy from control plane", "err", err)
		} else if cur := a.store.Get().RemotePolicy; cur == nil || !samePolicy(*cur, *p) {
			if err := a.store.Update(func(s *state.State) { s.RemotePolicy = p }); err == nil {
				a.log.Info("update policy changed", "enabled", p.Enabled, "scope", p.Scope)
			}
		}
	}
	for _, cmd := range resp.Commands {
		if a.markSeen(cmd.ID) {
			go a.execute(ctx, cmd)
		}
	}
}

// markSeen returns false for commands already executed, so a control plane
// that re-sends before receiving the result doesn't run them twice.
func (a *Agent) markSeen(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, t := range a.seenCommands {
		if time.Since(t) > time.Hour {
			delete(a.seenCommands, k)
		}
	}
	if id == "" {
		return false
	}
	if _, ok := a.seenCommands[id]; ok {
		return false
	}
	a.seenCommands[id] = time.Now()
	return true
}

func (a *Agent) addResult(r protocol.CommandResult) {
	a.mu.Lock()
	a.results = capTail(append(a.results, r), maxQueuedResults)
	a.mu.Unlock()
	a.poke(a.wake)
}

func (a *Agent) appsLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.Intervals.Apps)
	defer t.Stop()
	for {
		a.collectApps(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.appsKick:
		}
	}
}

func (a *Agent) collectApps(ctx context.Context) {
	if a.docker == nil && a.pm2 == nil && a.systemd == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out := &protocol.Apps{CollectedAt: time.Now().UTC()}
	var wg sync.WaitGroup
	if a.docker != nil {
		wg.Go(func() { out.Docker = a.docker.Collect(ctx) })
	}
	if a.pm2 != nil {
		wg.Go(func() { out.PM2 = a.pm2.Collect(ctx) })
	}
	if a.systemd != nil {
		wg.Go(func() { out.Systemd = a.systemd.Collect(ctx) })
	}
	wg.Wait()
	a.mu.Lock()
	a.apps, a.appsSent = out, false
	a.mu.Unlock()
}

func (a *Agent) updatesLoop(ctx context.Context) {
	// Let the first reports go out before hitting package mirrors.
	if !sleep(ctx, 30*time.Second) {
		return
	}
	check := func() {
		if err := a.updater.Check(ctx); err != nil && !errors.Is(err, updater.ErrBusy) {
			a.log.Warn("update check failed", "err", err)
		}
	}
	check()
	checkT := time.NewTicker(a.cfg.Intervals.UpdateCheck)
	schedT := time.NewTicker(time.Minute)
	defer checkT.Stop()
	defer schedT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-checkT.C:
			check()
		case now := <-schedT.C:
			a.updater.Tick(ctx, now)
		}
	}
}

func (a *Agent) poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func samePolicy(x, y protocol.Policy) bool {
	return x.Enabled == y.Enabled && x.Scope == y.Scope && x.AutoReboot == y.AutoReboot && x.IntervalHours == y.IntervalHours &&
		equalStrings(x.Exclude, y.Exclude) && sameWindow(x.Window, y.Window)
}

func sameWindow(x, y *protocol.Window) bool {
	if x == nil || y == nil {
		return x == y
	}
	return x.Start == y.Start && x.DurationMinutes == y.DurationMinutes && x.Timezone == y.Timezone && equalStrings(x.Days, y.Days)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func capTail[T any](s []T, n int) []T {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func nextBackoff(d time.Duration) time.Duration {
	return min(jitter(d*2), maxBackoff)
}

// jitter spreads reports from many hosts by ±10%.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
