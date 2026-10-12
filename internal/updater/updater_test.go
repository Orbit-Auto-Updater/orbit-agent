package updater

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/packages"
	"linux-dash-agent/internal/protocol"
	"linux-dash-agent/internal/state"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDueWindow(t *testing.T) {
	p := protocol.Policy{Enabled: true, Scope: "security", Window: &protocol.Window{Days: []string{"sun"}, Start: "23:30", DurationMinutes: 120, Timezone: "UTC"}}

	// 2024-10-13 is a Sunday.
	if _, due := Due(p, ts("2024-10-13T23:00:00Z"), time.Time{}, time.Time{}); due {
		t.Fatal("before window opens")
	}
	start, due := Due(p, ts("2024-10-13T23:45:00Z"), time.Time{}, time.Time{})
	if !due || !start.Equal(ts("2024-10-13T23:30:00Z")) {
		t.Fatalf("inside window: due=%v start=%v", due, start)
	}
	// Past midnight (Monday) but still inside Sunday's window.
	if _, due := Due(p, ts("2024-10-14T01:00:00Z"), time.Time{}, time.Time{}); !due {
		t.Fatal("window crossing midnight should still be open")
	}
	if _, due := Due(p, ts("2024-10-14T01:00:00Z"), start, time.Time{}); due {
		t.Fatal("same window must not run twice")
	}
	if _, due := Due(p, ts("2024-10-14T01:31:00Z"), time.Time{}, time.Time{}); due {
		t.Fatal("after window closes")
	}
	if _, due := Due(p, ts("2024-10-16T23:45:00Z"), time.Time{}, time.Time{}); due {
		t.Fatal("wrong day")
	}
	p.Enabled = false
	if _, due := Due(p, ts("2024-10-13T23:45:00Z"), time.Time{}, time.Time{}); due {
		t.Fatal("disabled policy")
	}
}

func TestDueWindowTimezone(t *testing.T) {
	p := protocol.Policy{Enabled: true, Scope: "all", Window: &protocol.Window{Start: "02:00", DurationMinutes: 60, Timezone: "America/New_York"}}
	// 02:30 EDT == 06:30 UTC
	if _, due := Due(p, ts("2024-10-13T06:30:00Z"), time.Time{}, time.Time{}); !due {
		t.Fatal("should be due at 02:30 New York time")
	}
	if _, due := Due(p, ts("2024-10-13T02:30:00Z"), time.Time{}, time.Time{}); due {
		t.Fatal("02:30 UTC is 22:30 in New York")
	}
}

func TestDueInterval(t *testing.T) {
	p := protocol.Policy{Enabled: true, Scope: "all", IntervalHours: 6}
	now := ts("2024-10-13T12:00:00Z")
	if _, due := Due(p, now, time.Time{}, time.Time{}); !due {
		t.Fatal("never run → due")
	}
	if _, due := Due(p, now, time.Time{}, now.Add(-5*time.Hour)); due {
		t.Fatal("ran 5h ago")
	}
	if _, due := Due(p, now, time.Time{}, now.Add(-6*time.Hour)); !due {
		t.Fatal("ran 6h ago")
	}
}

func newUpdater(t *testing.T, f *execx.Fake, freePct float64) *Updater {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := packages.Detect(f)
	if err != nil {
		t.Fatal(err)
	}
	return &Updater{
		Manager: mgr, Runner: f, Store: st, MinFreePct: 10, AllowReboot: true,
		FreePercent: func(string) (float64, error) { return freePct, nil },
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestApplySkipsWhenDiskLow(t *testing.T) {
	u := newUpdater(t, &execx.Fake{Binaries: map[string]bool{"apk": true}}, 4)
	run := u.Apply(context.Background(), "command", packages.UpgradeOptions{}, false)
	if run.Status != "skipped" || u.Store.Get().LastUpdateRun == nil {
		t.Fatalf("run = %+v", run)
	}
}

func TestApplySecurityOnlyFlow(t *testing.T) {
	f := &execx.Fake{Binaries: map[string]bool{"apk": true}, Responses: map[string]execx.Result{
		"apk update -q":             {},
		"apk list -u":               {Stdout: []byte("busybox-1.36.1-r29 x86_64 {busybox} (GPL-2.0-only) [upgradable from: busybox-1.36.1-r28]\n")},
		"apk upgrade --no-progress": {Stdout: []byte("(1/1) Upgrading busybox")},
	}}
	u := newUpdater(t, f, 50)
	run := u.Apply(context.Background(), "command", packages.UpgradeOptions{}, false)
	if run.Status != "success" || len(run.Packages) != 1 || run.Packages[0] != "busybox" {
		t.Fatalf("run = %+v", run)
	}
	s := u.Status()
	if s.Running || s.LastRun == nil || s.Manager != "apk" {
		t.Fatalf("status = %+v", s)
	}
}

func TestTickRunsOncePerWindow(t *testing.T) {
	f := &execx.Fake{Binaries: map[string]bool{"apk": true}, Responses: map[string]execx.Result{
		"apk update -q": {}, "apk list -u": {},
	}}
	u := newUpdater(t, f, 50)
	u.LocalPolicy = protocol.Policy{Enabled: true, Scope: "all", Window: &protocol.Window{Start: "02:00", DurationMinutes: 60, Timezone: "UTC"}}
	now := ts("2024-10-13T02:10:00Z")
	u.Tick(context.Background(), now)
	u.Tick(context.Background(), now.Add(10*time.Minute))
	n := 0
	for _, c := range f.Calls {
		if c == "apk update -q" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected one scheduled run, got %d (calls %v)", n, f.Calls)
	}
}

func TestRemotePolicyOverridesLocal(t *testing.T) {
	u := newUpdater(t, &execx.Fake{Binaries: map[string]bool{"apk": true}}, 50)
	u.LocalPolicy = protocol.Policy{Scope: "security"}
	remote := &protocol.Policy{Enabled: true, Scope: "all", IntervalHours: 6}
	u.Store.Update(func(s *state.State) { s.RemotePolicy = remote })
	if u.Policy().Scope != "security" {
		t.Fatal("remote policy must be ignored unless allowed")
	}
	u.AllowRemote = true
	if u.Policy().Scope != "all" {
		t.Fatal("remote policy should apply")
	}
}
