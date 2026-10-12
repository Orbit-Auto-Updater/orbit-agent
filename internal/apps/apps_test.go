package apps

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"linux-dash-agent/internal/execx"
)

func TestParsePM2JList(t *testing.T) {
	out := []byte(`>>>> In-memory PM2 is out-of-date, do:
>>>> $ pm2 update
[{"pid":4123,"name":"storefront-ssr","pm_id":0,"monit":{"memory":52428800,"cpu":12.5},
  "pm2_env":{"status":"online","exec_mode":"cluster_mode","restart_time":3,"unstable_restarts":0,"pm_uptime":1728600000000}},
 {"pid":0,"name":"webhooks","pm_id":1,"monit":{"memory":0,"cpu":0},
  "pm2_env":{"status":"errored","exec_mode":"fork_mode","restart_time":27,"unstable_restarts":15,"pm_uptime":1728600000000}}]`)
	procs, err := ParsePM2JList(out, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 2 {
		t.Fatalf("want 2, got %d", len(procs))
	}
	a, b := procs[0], procs[1]
	if a.Name != "storefront-ssr" || a.Mode != "cluster" || a.MemBytes != 52428800 || a.Restarts != 3 || a.User != "deploy" || a.StartedAt.IsZero() {
		t.Fatalf("a = %+v", a)
	}
	if b.Status != "errored" || b.Mode != "fork" || !b.StartedAt.IsZero() {
		t.Fatalf("b = %+v", b)
	}
	if p, err := ParsePM2JList([]byte("\n"), ""); err != nil || len(p) != 0 {
		t.Fatalf("empty output: %v %v", p, err)
	}
}

func TestParseSystemctl(t *testing.T) {
	show := "Id=nginx.service\nDescription=A high performance web server\nActiveState=active\nSubState=running\nUnitFileState=enabled\nMainPID=812\nNRestarts=0\n\nId=fail2ban.service\nDescription=Fail2Ban Service\nActiveState=failed\nSubState=failed\nUnitFileState=enabled\nMainPID=0\nNRestarts=2\n"
	units := ParseSystemctlShow(show)
	if len(units) != 2 || units[0].MainPID != 812 || units[1].ActiveState != "failed" || units[1].Restarts != 2 {
		t.Fatalf("%+v", units)
	}

	failed := ParseFailedUnits("fail2ban.service loaded failed failed Fail2Ban Service\n● certbot.service loaded failed failed Certbot renewal\n")
	if len(failed) != 2 || failed[1].Name != "certbot.service" || failed[0].Description != "Fail2Ban Service" {
		t.Fatalf("%+v", failed)
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{"-rf", "--force", "a b", "x;rm", "../etc", "", "$(id)"} {
		if validName(bad) || validUnit(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, good := range []string{"nginx.service", "getty@tty1.service", "api_gateway", "3f2a9c1b2d4e"} {
		if !validName(good) && !validUnit(good) {
			t.Errorf("%q should be accepted", good)
		}
	}
}

func TestSystemdControlRequiresWatchedUnit(t *testing.T) {
	f := &execx.Fake{Responses: map[string]execx.Result{"systemctl restart --no-block -- nginx.service": {}}}
	s := &Systemd{Watch: []string{"nginx.service"}, Runner: f}
	if err := s.Control(context.Background(), "restart", "nginx.service"); err != nil {
		t.Fatal(err)
	}
	if err := s.Control(context.Background(), "restart", "sshd.service"); err == nil {
		t.Fatal("unwatched unit should be refused")
	}
	if err := s.Control(context.Background(), "mask", "nginx.service"); err == nil {
		t.Fatal("unsupported action should be refused")
	}
}

func TestDockerCollect(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "dk") // short path: unix socket names are length-limited
	if err != nil {
		t.Skip("cannot create socket dir:", err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Id":"3f2a9c1b2d4e5f60718293a4","Names":["/api"],"Image":"acme/api:1","State":"running","Status":"Up 2 hours","Created":1728600000,
		  "Ports":[{"IP":"0.0.0.0","PrivatePort":8080,"PublicPort":80,"Type":"tcp"}]},
		 {"Id":"aaaaaaaaaaaaaaaa","Names":["/cron"],"Image":"acme/cron","State":"exited","Status":"Exited (1)","Created":1728600000,"Ports":[]}]`))
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"RestartCount":4,"State":{"ExitCode":137,"OOMKilled":true,"StartedAt":"2024-10-11T08:00:00Z","Health":{"Status":"healthy"}}}`))
	})
	mux.HandleFunc("GET /containers/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cpu_stats":{"cpu_usage":{"total_usage":2000},"system_cpu_usage":20000,"online_cpus":4},
		  "precpu_stats":{"cpu_usage":{"total_usage":1000},"system_cpu_usage":10000},
		  "memory_stats":{"usage":1000,"limit":4000,"stats":{"inactive_file":200}}}`))
	})
	mux.HandleFunc("POST /containers/{id}/restart", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	d := NewDocker(sock)
	st := d.Collect(context.Background())
	if st.Error != "" {
		t.Fatal(st.Error)
	}
	api := st.Containers[0]
	if api.Name != "api" || api.ID != "3f2a9c1b2d4e" || api.CPUPercent != 40 || api.MemBytes != 800 || api.RestartCount != 4 || !api.OOMKilled || api.Health != "healthy" {
		t.Fatalf("api = %+v", api)
	}
	if strings.Join(api.Ports, ",") != "80->8080/tcp" {
		t.Fatalf("ports = %v", api.Ports)
	}
	if st.Containers[1].CPUPercent != 0 || st.Containers[1].State != "exited" {
		t.Fatalf("stopped container = %+v", st.Containers[1])
	}
	if err := d.Control(context.Background(), "restart", "api"); err != nil {
		t.Fatal(err)
	}
	if err := d.Control(context.Background(), "restart", "--all"); err == nil {
		t.Fatal("flag-like id should be refused")
	}
	if err := d.Control(context.Background(), "kill", "api"); err == nil {
		t.Fatal("unsupported action should be refused")
	}
}
