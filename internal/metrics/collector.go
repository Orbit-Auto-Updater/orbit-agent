// Package metrics reads host resource usage from /proc and statfs.
package metrics

import (
	"bytes"
	"math"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"orbit-agent/internal/protocol"
)

type Collector struct {
	ProcRoot          string // default /proc
	ExcludeInterfaces []string
	ExcludeMounts     []string

	mu       sync.Mutex
	prevCPU  CPUTimes
	prevNet  map[string]NetCounters
	prevTime time.Time
}

func NewCollector(excludeIfaces, excludeMounts []string) *Collector {
	return &Collector{ProcRoot: "/proc", ExcludeInterfaces: excludeIfaces, ExcludeMounts: excludeMounts}
}

func (c *Collector) read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(c.ProcRoot, name))
}

// Collect samples all metrics. CPU and network rates are computed against the
// previous call, so the first call reports zero rates.
func (c *Collector) Collect() *protocol.Metrics {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	m := &protocol.Metrics{CollectedAt: now.UTC()}
	fail := func(err error) { m.Errors = append(m.Errors, err.Error()) }

	if b, err := c.read("stat"); err != nil {
		fail(err)
	} else if cur, err := ParseProcStat(bytes.NewReader(b)); err != nil {
		fail(err)
	} else {
		if !c.prevTime.IsZero() {
			m.CPUPercent, m.CPUIOWaitPercent = CPUUsage(c.prevCPU, cur)
		}
		c.prevCPU = cur
	}

	if b, err := c.read("meminfo"); err != nil {
		fail(err)
	} else if mi, err := ParseMeminfo(bytes.NewReader(b)); err != nil {
		fail(err)
	} else {
		m.MemTotalBytes = mi["MemTotal"]
		m.MemAvailBytes = mi["MemAvailable"]
		if m.MemAvailBytes == 0 { // kernels < 3.14
			m.MemAvailBytes = mi["MemFree"] + mi["Buffers"] + mi["Cached"]
		}
		m.MemUsedBytes = m.MemTotalBytes - min(m.MemAvailBytes, m.MemTotalBytes)
		if m.MemTotalBytes > 0 {
			m.MemPercent = round1(float64(m.MemUsedBytes) / float64(m.MemTotalBytes) * 100)
		}
		m.SwapTotalBytes = mi["SwapTotal"]
		m.SwapUsedBytes = m.SwapTotalBytes - min(mi["SwapFree"], m.SwapTotalBytes)
	}

	if b, err := c.read("loadavg"); err != nil {
		fail(err)
	} else if l1, l5, l15, err := ParseLoadavg(string(b)); err != nil {
		fail(err)
	} else {
		m.Load1, m.Load5, m.Load15 = l1, l5, l15
	}

	if b, err := c.read("uptime"); err == nil {
		m.UptimeSeconds, _ = ParseUptime(string(b))
	}

	if b, err := c.read("net/dev"); err != nil {
		fail(err)
	} else if counters, err := ParseNetDev(bytes.NewReader(b)); err != nil {
		fail(err)
	} else {
		elapsed := now.Sub(c.prevTime).Seconds()
		for name, cur := range counters {
			if matchAny(c.ExcludeInterfaces, name) {
				continue
			}
			ni := protocol.NetIface{Name: name, RxBytesTotal: cur.RxBytes, TxBytesTotal: cur.TxBytes, RxErrors: cur.RxErrors, TxErrors: cur.TxErrors}
			if prev, ok := c.prevNet[name]; ok && elapsed > 0 {
				ni.RxBytesPerSec = rate(prev.RxBytes, cur.RxBytes, elapsed)
				ni.TxBytesPerSec = rate(prev.TxBytes, cur.TxBytes, elapsed)
			}
			m.Net = append(m.Net, ni)
		}
		c.prevNet = counters
	}

	if b, err := c.read("self/mounts"); err != nil {
		fail(err)
	} else if mounts, err := ParseMounts(bytes.NewReader(b)); err != nil {
		fail(err)
	} else {
		for _, mt := range mounts {
			if matchAny(c.ExcludeMounts, mt.Point) {
				continue
			}
			d, err := statDisk(mt)
			if err != nil {
				continue // e.g. permission denied on a FUSE mount
			}
			m.Disks = append(m.Disks, d)
		}
	}

	c.prevTime = now
	m.CPUPercent = round1(m.CPUPercent)
	m.CPUIOWaitPercent = round1(m.CPUIOWaitPercent)
	return m
}

// HostInfo gathers static host facts.
func (c *Collector) HostInfo() protocol.HostInfo {
	h := protocol.HostInfo{Arch: runtime.GOARCH, CPUCores: runtime.NumCPU()}
	h.Hostname, _ = os.Hostname()
	if f, err := os.Open("/etc/os-release"); err == nil {
		osr := ParseOSRelease(f)
		f.Close()
		h.OSName, h.OSID, h.OSVersion = osr["PRETTY_NAME"], osr["ID"], osr["VERSION_ID"]
	}
	if b, err := c.read("sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	if b, err := c.read("meminfo"); err == nil {
		if mi, err := ParseMeminfo(bytes.NewReader(b)); err == nil {
			h.MemTotalBytes = mi["MemTotal"]
		}
	}
	if b, err := c.read("uptime"); err == nil {
		if up, err := ParseUptime(string(b)); err == nil {
			h.BootTime = time.Now().Add(-time.Duration(up * float64(time.Second))).UTC().Truncate(time.Second)
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				h.IPs = append(h.IPs, ipn.IP.String())
			}
		}
	}
	return h
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

func rate(prev, cur uint64, secs float64) float64 {
	if cur < prev { // counter reset / wrap
		return 0
	}
	return math.Round(float64(cur-prev) / secs)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
