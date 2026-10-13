package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orbit-agent/internal/config"
	"orbit-agent/internal/protocol"
	"orbit-agent/internal/state"
)

// fakePlane is a minimal control plane for exercising the full agent loop.
type fakePlane struct {
	progress []protocol.Progress
	mu       sync.Mutex
	enrolls  int
	reports  []protocol.Report
	authOK   bool
	commands []protocol.Command
	policy   *protocol.Policy
}

func (f *fakePlane) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	read := func(r *http.Request, v any) {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("request not gzipped: %v", err)
			return
		}
		if err := json.NewDecoder(gz).Decode(v); err != nil {
			t.Errorf("decode: %v", err)
		}
	}
	mux.HandleFunc("POST "+protocol.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
		var req protocol.EnrollRequest
		read(r, &req)
		if req.Token != "good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.enrolls++
		f.mu.Unlock()
		json.NewEncoder(w).Encode(protocol.EnrollResponse{AgentID: "agt_1", Secret: "s3cret"})
	})
	mux.HandleFunc("POST "+protocol.PathProgress, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var p protocol.Progress
		read(r, &p)
		f.mu.Lock()
		f.progress = append(f.progress, p)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+protocol.PathReport, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-ID") != "agt_1" || r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var rep protocol.Report
		read(r, &rep)
		f.mu.Lock()
		f.authOK = true
		f.reports = append(f.reports, rep)
		resp := protocol.ReportResponse{Commands: f.commands, Policy: f.policy}
		f.commands, f.policy = nil, nil
		// Re-deliver the first command once to check de-duplication.
		if len(f.reports) == 1 && len(resp.Commands) > 0 {
			f.commands = resp.Commands[:1]
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func (f *fakePlane) results() []protocol.CommandResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []protocol.CommandResult
	for _, r := range f.reports {
		out = append(out, r.Results...)
	}
	return out
}

func TestAgentEndToEnd(t *testing.T) {
	plane := &fakePlane{
		commands: []protocol.Command{
			{ID: "c1", Type: protocol.CmdAgentRefresh},
			{ID: "c2", Type: protocol.CmdHostReboot}, // actions.reboot defaults to false
			{ID: "c3", Type: "shell.exec", Args: json.RawMessage(`{"cmd":"id"}`)},
			{ID: "c4", Type: protocol.CmdDockerRestart, Args: json.RawMessage(`{"id":"x","extra":1}`)},
			{ID: "c5", Type: protocol.CmdUpdatesApply, Args: json.RawMessage(`{"security_only":true}`)},
		},
		policy: &protocol.Policy{Enabled: true, Scope: "all", IntervalHours: 12},
	}
	srv := httptest.NewServer(plane.handler(t))
	defer srv.Close()

	cfg := config.Default()
	cfg.Server.URL, cfg.Server.EnrollToken, cfg.Server.AllowInsecureHTTP = srv.URL, "good-token", true
	cfg.Intervals.Report = 2 * time.Second
	cfg.Updates.Enabled = true
	cfg.Docker.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(cfg, store, log, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	deadline := time.Now().Add(8 * time.Second)
	for len(plane.results()) < 5 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	byID := map[string]protocol.CommandResult{}
	for _, r := range plane.results() {
		if _, dup := byID[r.ID]; dup {
			t.Errorf("command %s executed twice", r.ID)
		}
		byID[r.ID] = r
	}
	if r := byID["c1"]; !r.OK {
		t.Errorf("agent.refresh should succeed: %+v", r)
	}
	if r := byID["c2"]; r.OK || r.Error == "" {
		t.Errorf("reboot must be refused by local config: %+v", r)
	}
	if r := byID["c3"]; r.OK {
		t.Errorf("unknown command type must be refused: %+v", r)
	}
	if r := byID["c4"]; r.OK {
		t.Errorf("unknown args / missing docker must be refused: %+v", r)
	}

	// updates.apply streams live progress tagged with its command, ending with
	// a final chunk (this test host has no package manager, so it fails).
	plane.mu.Lock()
	var final *protocol.Progress
	lines := 0
	for i := range plane.progress {
		p := plane.progress[i]
		if p.CommandID != "c5" || p.Trigger != "command" || p.RunID == "" {
			t.Errorf("unexpected progress chunk %+v", p)
		}
		lines += len(p.Lines)
		if p.Done {
			final = &p
		}
	}
	plane.mu.Unlock()
	if final == nil || final.Status != "failed" || !strings.Contains(final.Error, "package manager") || lines == 0 {
		t.Errorf("final progress = %+v (lines %d)", final, lines)
	}

	st := store.Get()
	if st.AgentID != "agt_1" || st.Secret != "s3cret" || plane.enrolls != 1 {
		t.Errorf("enrolment not persisted: %+v enrolls=%d", st, plane.enrolls)
	}
	if st.RemotePolicy == nil || st.RemotePolicy.Scope != "all" {
		t.Errorf("policy from control plane not stored: %+v", st.RemotePolicy)
	}
	first := plane.reports[0]
	if first.Metrics == nil || first.AgentVersion != "test" || first.Host.Hostname == "" {
		t.Errorf("first report incomplete: %+v", first)
	}
}

func TestEnrolRequiresToken(t *testing.T) {
	cfg := config.Default()
	cfg.Server.URL, cfg.Server.AllowInsecureHTTP = "http://127.0.0.1:1", true
	store, _ := state.Open(t.TempDir())
	a, err := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("expected error without enroll token")
	}
}
