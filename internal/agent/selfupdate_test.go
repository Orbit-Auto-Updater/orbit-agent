package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"linux-dash-agent/internal/config"
	"linux-dash-agent/internal/protocol"
	"linux-dash-agent/internal/state"
)

// newBinary is a stand-in "agent release": answers `version` and `validate`.
const newBinary = "#!/bin/sh\ncase \"$1\" in version) echo 9.9.9;; validate) echo config ok;; esac\n"

func selfUpdateAgent(t *testing.T, plane *fakePlane, extra http.Handler) (*Agent, string, *httptest.Server) {
	t.Helper()
	t.Setenv("INVOCATION_ID", "test") // pretend systemd supervises us
	mux := http.NewServeMux()
	mux.Handle("/api/", plane.handler(t))
	if extra != nil {
		mux.Handle("/downloads/", extra)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	exe := filepath.Join(dir, "linux-dash-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Server.URL, cfg.Server.EnrollToken, cfg.Server.AllowInsecureHTTP = srv.URL, "good-token", true
	cfg.Intervals.Report = 2 * time.Second
	cfg.Updates.Enabled, cfg.Docker.Enabled = false, false
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	a.executable = func() (string, error) { return exe, nil }
	a.ConfigPath = "/etc/linux-dash-agent/config.yaml"
	return a, exe, srv
}

func serveBinary(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/downloads/linux-dash-agent-linux-"+runtime.GOARCH {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	})
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func updateCmd(id, checksum string) protocol.Command {
	args, _ := json.Marshal(protocol.AgentUpdateArgs{Version: "9.9.9", Path: "/downloads/linux-dash-agent-linux-" + runtime.GOARCH, SHA256: checksum})
	return protocol.Command{ID: id, Type: protocol.CmdAgentUpdate, Args: args}
}

func TestSelfUpdateInstallsAndRestartsAfterReporting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	plane := &fakePlane{commands: []protocol.Command{updateCmd("u1", sum(newBinary))}}
	a, exe, _ := selfUpdateAgent(t, plane, serveBinary(newBinary))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := a.Run(ctx)
	if err != ErrRestart {
		t.Fatalf("Run = %v, want ErrRestart", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != newBinary {
		t.Fatalf("binary not replaced: %q", b)
	}
	if b, _ := os.ReadFile(exe + ".previous"); string(b) != "#!/bin/sh\necho 1.0.0\n" {
		t.Fatalf("previous binary not kept: %q", b)
	}
	p := a.store.Get().PendingUpdate
	if p == nil || p.From != "1.0.0" || p.To != "9.9.9" {
		t.Fatalf("pending update = %+v", p)
	}
	// It restarted only after the result reached the dashboard.
	var ok bool
	for _, r := range plane.results() {
		ok = ok || (r.ID == "u1" && r.OK)
	}
	if !ok {
		t.Fatal("update result was not reported before restarting")
	}
	plane.mu.Lock()
	var final *protocol.Progress
	for i := range plane.progress {
		if plane.progress[i].Kind == protocol.CmdAgentUpdate && plane.progress[i].Done {
			final = &plane.progress[i]
		}
	}
	plane.mu.Unlock()
	if final == nil || final.Status != "success" {
		t.Fatalf("final progress = %+v", final)
	}
	if info := a.agentInfo(); !info.SelfUpdate || info.Supervisor != "systemd" || !info.Actions["agent_update"] {
		t.Fatalf("agent info = %+v", info)
	}
}

func TestSelfUpdateRejectsBadChecksum(t *testing.T) {
	plane := &fakePlane{commands: []protocol.Command{updateCmd("u2", sum("something else"))}}
	a, exe, _ := selfUpdateAgent(t, plane, serveBinary(newBinary))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go a.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for len(plane.results()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	res := plane.results()
	if len(res) == 0 || res[0].OK || res[0].Error == "" {
		t.Fatalf("expected failed result, got %+v", res)
	}
	if b, _ := os.ReadFile(exe); string(b) != "#!/bin/sh\necho 1.0.0\n" {
		t.Fatal("binary must be untouched after a failed verification")
	}
	if a.store.Get().PendingUpdate != nil {
		t.Fatal("no pending update expected")
	}
}

func TestPendingUpdateConfirmedAndRolledBack(t *testing.T) {
	// Confirmation: the new version reports in successfully.
	plane := &fakePlane{}
	a, exe, _ := selfUpdateAgent(t, plane, nil)
	a.version = "9.9.9"
	os.WriteFile(exe+".previous", []byte("old"), 0o755)
	a.store.Update(func(s *state.State) {
		s.PendingUpdate = &state.PendingUpdate{From: "1.0.0", To: "9.9.9", Backup: exe + ".previous", Binary: exe, At: time.Now()}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	go a.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for a.store.Get().PendingUpdate != nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if st := a.store.Get(); st.PendingUpdate != nil || st.LastSelfUpdate == nil || st.LastSelfUpdate.Status != "confirmed" {
		t.Fatalf("expected confirmation, state = %+v", st)
	}
	if _, err := os.Stat(exe + ".previous"); !os.IsNotExist(err) {
		t.Fatal("backup should be removed once confirmed")
	}

	// Rollback: the new version keeps restarting without reporting in.
	b, exe2, _ := selfUpdateAgent(t, &fakePlane{}, nil)
	b.version = "9.9.9"
	os.WriteFile(exe2, []byte("new"), 0o755)
	os.WriteFile(exe2+".previous", []byte("old"), 0o755)
	b.store.Update(func(s *state.State) {
		s.PendingUpdate = &state.PendingUpdate{From: "1.0.0", To: "9.9.9", Backup: exe2 + ".previous", Binary: exe2, At: time.Now(), Failures: maxUnconfirmed}
	})
	if err := b.checkPendingUpdate(); err != ErrRestart {
		t.Fatalf("checkPendingUpdate = %v, want ErrRestart", err)
	}
	if got, _ := os.ReadFile(exe2); string(got) != "old" {
		t.Fatalf("previous binary not restored: %q", got)
	}
	if st := b.store.Get(); st.LastSelfUpdate == nil || st.LastSelfUpdate.Status != "rolled_back" {
		t.Fatalf("state = %+v", st)
	}
}
