// Package apps monitors and controls Docker containers, PM2 processes and
// systemd units.
package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"linux-dash-agent/internal/protocol"
)

// Docker talks to the Engine API over its Unix socket; no SDK required.
type Docker struct {
	Socket string
	client *http.Client
}

func NewDocker(socket string) *Docker {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns: 4,
	}
	return &Docker{Socket: socket, client: &http.Client{Transport: tr, Timeout: 20 * time.Second}}
}

func (d *Docker) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		var e struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		return fmt.Errorf("docker %s %s: %s %s", method, path, resp.Status, e.Message)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type dockerListItem struct {
	ID      string   `json:"Id"`
	Names   []string `json:"Names"`
	Image   string   `json:"Image"`
	State   string   `json:"State"`
	Status  string   `json:"Status"`
	Created int64    `json:"Created"`
	Ports   []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

type dockerInspect struct {
	RestartCount int `json:"RestartCount"`
	State        struct {
		ExitCode  int       `json:"ExitCode"`
		OOMKilled bool      `json:"OOMKilled"`
		StartedAt time.Time `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

type dockerStats struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  int    `json:"online_cpus"`
}

// cpuPercent mirrors the calculation used by `docker stats`.
func (s dockerStats) cpuPercent() float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	cpus := s.CPUStats.OnlineCPUs
	if cpus == 0 {
		cpus = len(s.CPUStats.CPUUsage.PercpuUsage)
	}
	if cpuDelta <= 0 || sysDelta <= 0 || cpus == 0 {
		return 0
	}
	return round1(cpuDelta / sysDelta * float64(cpus) * 100)
}

// memUsage excludes page cache, as `docker stats` does (cgroup v1 and v2).
func (s dockerStats) memUsage() uint64 {
	cache := s.MemoryStats.Stats["inactive_file"]
	if v, ok := s.MemoryStats.Stats["total_inactive_file"]; ok {
		cache = v
	}
	if cache > s.MemoryStats.Usage {
		return s.MemoryStats.Usage
	}
	return s.MemoryStats.Usage - cache
}

func (d *Docker) Collect(ctx context.Context) *protocol.DockerStatus {
	st := &protocol.DockerStatus{Containers: []protocol.Container{}}
	var list []dockerListItem
	if err := d.do(ctx, http.MethodGet, "/containers/json?all=1", &list); err != nil {
		st.Error = err.Error()
		return st
	}

	st.Containers = make([]protocol.Container, len(list))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, it := range list {
		c := protocol.Container{
			ID:        shortID(it.ID),
			Name:      strings.TrimPrefix(first(it.Names), "/"),
			Image:     it.Image,
			State:     it.State,
			Status:    it.Status,
			CreatedAt: time.Unix(it.Created, 0).UTC(),
		}
		for _, p := range it.Ports {
			c.Ports = append(c.Ports, formatPort(p.IP, p.PublicPort, p.PrivatePort, p.Type))
		}
		st.Containers[i] = c

		wg.Add(1)
		go func(i int, id string, running bool) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c := &st.Containers[i]
			var ins dockerInspect
			if err := d.do(ctx, http.MethodGet, "/containers/"+id+"/json", &ins); err == nil {
				c.RestartCount, c.ExitCode, c.OOMKilled = ins.RestartCount, ins.State.ExitCode, ins.State.OOMKilled
				if !ins.State.StartedAt.IsZero() && ins.State.StartedAt.Year() > 1 {
					c.StartedAt = ins.State.StartedAt.UTC()
				}
				if ins.State.Health != nil {
					c.Health = ins.State.Health.Status
				}
			}
			if running {
				// stream=false waits one sampling period so precpu is populated.
				var s dockerStats
				if err := d.do(ctx, http.MethodGet, "/containers/"+id+"/stats?stream=false", &s); err == nil {
					c.CPUPercent, c.MemBytes, c.MemLimit = s.cpuPercent(), s.memUsage(), s.MemoryStats.Limit
				}
			}
		}(i, it.ID, it.State == "running")
	}
	wg.Wait()
	return st
}

// Control starts, stops or restarts a container by ID or name.
func (d *Docker) Control(ctx context.Context, action, id string) error {
	if !validName(id) {
		return fmt.Errorf("invalid container id %q", id)
	}
	switch action {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("unsupported docker action %q", action)
	}
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/"+action, nil)
}

func formatPort(ip string, public, private int, typ string) string {
	if public == 0 {
		return fmt.Sprintf("%d/%s", private, typ)
	}
	if ip == "" || ip == "0.0.0.0" || ip == "::" {
		return fmt.Sprintf("%d->%d/%s", public, private, typ)
	}
	return fmt.Sprintf("%s:%d->%d/%s", ip, public, private, typ)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
