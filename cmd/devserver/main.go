// Command devserver is an in-memory reference implementation of the control
// plane side of the agent protocol. Use it to develop and test the agent
// locally, and as a template for the dashboard's API routes.
//
//	go run ./cmd/devserver -addr :8080 -token dev-token
//
// Queue a command for an agent:
//
//	curl -X POST localhost:8080/dev/agents/<id>/commands \
//	     -d '{"type":"docker.restart","args":{"id":"nginx"}}'
package main

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"linux-dash-agent/internal/protocol"
)

type agentRecord struct {
	ID       string `json:"id"`
	secret   string
	Host     protocol.HostInfo        `json:"host"`
	LastSeen time.Time                `json:"last_seen"`
	Last     *protocol.Report         `json:"last_report,omitempty"`
	Apps     *protocol.Apps           `json:"apps,omitempty"`
	Updates  *protocol.UpdateStatus   `json:"updates,omitempty"`
	Logs     []protocol.LogEntry      `json:"recent_logs,omitempty"`
	Results  []protocol.CommandResult `json:"results,omitempty"`
	Pending  []protocol.Command       `json:"pending_commands,omitempty"`
	Policy   *protocol.Policy         `json:"policy,omitempty"`
	policyTx bool                     // policy changed and not yet delivered
}

type server struct {
	token  string
	mu     sync.Mutex
	agents map[string]*agentRecord
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	token := flag.String("token", "dev-token", "enrolment token agents must present")
	flag.Parse()

	s := &server{token: *token, agents: map[string]*agentRecord{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+protocol.PathEnroll, s.enroll)
	mux.HandleFunc("POST "+protocol.PathReport, s.report)
	mux.HandleFunc("GET /dev/agents", s.list)
	mux.HandleFunc("GET /dev/agents/{id}", s.get)
	mux.HandleFunc("POST /dev/agents/{id}/commands", s.queue)
	mux.HandleFunc("PUT /dev/agents/{id}/policy", s.setPolicy)

	log.Printf("devserver listening on %s (enrol token %q)", *addr, *token)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func decode(r *http.Request, v any) error {
	var body io.Reader = http.MaxBytesReader(nil, r.Body, 16<<20)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return err
		}
		defer gz.Close()
		body = gz
	}
	return json.NewDecoder(body).Decode(v)
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *server) enroll(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrollRequest
	if err := decode(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
		http.Error(w, "invalid enrolment token", http.StatusUnauthorized)
		return
	}
	rec := &agentRecord{ID: "agt_" + randomHex(8), secret: randomHex(32), Host: req.Host, LastSeen: time.Now()}
	s.mu.Lock()
	s.agents[rec.ID] = rec
	s.mu.Unlock()
	log.Printf("enrolled %s: %s (%s, %s)", rec.ID, req.Host.Hostname, req.Host.OSName, req.Host.Arch)
	reply(w, http.StatusOK, protocol.EnrollResponse{AgentID: rec.ID, Secret: rec.secret})
}

func (s *server) authed(r *http.Request) *agentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.agents[r.Header.Get("X-Agent-ID")]
	if rec == nil {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+rec.secret)) != 1 {
		return nil
	}
	return rec
}

func (s *server) report(w http.ResponseWriter, r *http.Request) {
	rec := s.authed(r)
	if rec == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var rep protocol.Report
	if err := decode(r, &rep); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	rec.LastSeen, rec.Host, rec.Last = time.Now(), rep.Host, &rep
	if rep.Apps != nil {
		rec.Apps = rep.Apps
	}
	if rep.Updates != nil {
		rec.Updates = rep.Updates
	}
	rec.Logs = append(rec.Logs, rep.Logs...)
	if len(rec.Logs) > 200 {
		rec.Logs = rec.Logs[len(rec.Logs)-200:]
	}
	rec.Results = append(rec.Results, rep.Results...)
	// Commands are delivered until the agent reports a result for them.
	for _, res := range rep.Results {
		for i, c := range rec.Pending {
			if c.ID == res.ID {
				rec.Pending = append(rec.Pending[:i], rec.Pending[i+1:]...)
				break
			}
		}
	}
	resp := protocol.ReportResponse{Commands: rec.Pending}
	if rec.policyTx {
		resp.Policy, rec.policyTx = rec.Policy, false
	}
	s.mu.Unlock()

	logReport(rec.ID, &rep)
	reply(w, http.StatusOK, resp)
}

func logReport(id string, rep *protocol.Report) {
	line := fmt.Sprintf("%s %-16s", id, rep.Host.Hostname)
	if m := rep.Metrics; m != nil {
		line += fmt.Sprintf(" cpu=%5.1f%% mem=%5.1f%% load=%.2f", m.CPUPercent, m.MemPercent, m.Load1)
		var rx, tx float64
		for _, n := range m.Net {
			rx, tx = rx+n.RxBytesPerSec, tx+n.TxBytesPerSec
		}
		line += fmt.Sprintf(" net=↓%s/s ↑%s/s", human(rx), human(tx))
	}
	if a := rep.Apps; a != nil {
		if a.Docker != nil {
			line += fmt.Sprintf(" containers=%d", len(a.Docker.Containers))
		}
		if a.PM2 != nil {
			line += fmt.Sprintf(" pm2=%d", len(a.PM2.Processes))
		}
		if a.Systemd != nil {
			line += fmt.Sprintf(" units=%d", len(a.Systemd.Units))
		}
	}
	if u := rep.Updates; u != nil {
		line += fmt.Sprintf(" updates=%d reboot=%v", len(u.Packages), u.RebootRequired)
	}
	if len(rep.Logs) > 0 {
		line += fmt.Sprintf(" logs=%d", len(rep.Logs))
	}
	log.Print(line)
	for _, r := range rep.Results {
		log.Printf("  result %s %s ok=%v %s%s", r.ID, r.Type, r.OK, r.Error, firstLine(r.Output))
	}
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type summary struct {
		ID       string    `json:"id"`
		Hostname string    `json:"hostname"`
		OS       string    `json:"os"`
		LastSeen time.Time `json:"last_seen"`
		Online   bool      `json:"online"`
		CPU      float64   `json:"cpu_percent"`
		Mem      float64   `json:"mem_percent"`
		Updates  int       `json:"pending_updates"`
	}
	out := []summary{}
	for _, a := range s.agents {
		sm := summary{ID: a.ID, Hostname: a.Host.Hostname, OS: a.Host.OSName, LastSeen: a.LastSeen, Online: time.Since(a.LastSeen) < time.Minute}
		if a.Last != nil && a.Last.Metrics != nil {
			sm.CPU, sm.Mem = a.Last.Metrics.CPUPercent, a.Last.Metrics.MemPercent
		}
		if a.Updates != nil {
			sm.Updates = len(a.Updates.Packages)
		}
		out = append(out, sm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	reply(w, http.StatusOK, out)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.agents[r.PathValue("id")]
	if rec == nil {
		http.NotFound(w, r)
		return
	}
	reply(w, http.StatusOK, rec)
}

func (s *server) queue(w http.ResponseWriter, r *http.Request) {
	var cmd protocol.Command
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil || cmd.Type == "" {
		http.Error(w, "body must be {\"type\": ..., \"args\": {...}}", http.StatusBadRequest)
		return
	}
	cmd.ID, cmd.IssuedAt = "cmd_"+randomHex(6), time.Now().UTC()
	s.mu.Lock()
	rec := s.agents[r.PathValue("id")]
	if rec != nil {
		rec.Pending = append(rec.Pending, cmd)
	}
	s.mu.Unlock()
	if rec == nil {
		http.NotFound(w, r)
		return
	}
	log.Printf("queued %s %s for %s", cmd.ID, cmd.Type, rec.ID)
	reply(w, http.StatusAccepted, cmd)
}

func (s *server) setPolicy(w http.ResponseWriter, r *http.Request) {
	var p protocol.Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	rec := s.agents[r.PathValue("id")]
	if rec != nil {
		rec.Policy, rec.policyTx = &p, true
	}
	s.mu.Unlock()
	if rec == nil {
		http.NotFound(w, r)
		return
	}
	reply(w, http.StatusOK, p)
}

func human(b float64) string {
	units := []string{"B", "KB", "MB", "GB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	return fmt.Sprintf("%.1f%s", b, units[i])
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return " — " + s[:i]
		}
	}
	if s == "" {
		return ""
	}
	return " — " + s
}
