package metrics

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CPUTimes is the aggregate "cpu" line of /proc/stat, in jiffies.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

func (t CPUTimes) total() uint64 {
	// guest/guest_nice are already included in user/nice.
	return t.User + t.Nice + t.System + t.Idle + t.IOWait + t.IRQ + t.SoftIRQ + t.Steal
}

// CPUUsage returns busy% and iowait% between two samples.
func CPUUsage(prev, cur CPUTimes) (busy, iowait float64) {
	dt := float64(cur.total()) - float64(prev.total())
	if dt <= 0 {
		return 0, 0
	}
	idle := float64(cur.Idle+cur.IOWait) - float64(prev.Idle+prev.IOWait)
	busy = clampPct((dt - idle) / dt * 100)
	iowait = clampPct((float64(cur.IOWait) - float64(prev.IOWait)) / dt * 100)
	return busy, iowait
}

func ParseProcStat(r io.Reader) (CPUTimes, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		v := make([]uint64, 8)
		for i := 0; i < 8 && i+1 < len(f); i++ {
			n, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				return CPUTimes{}, fmt.Errorf("parse /proc/stat: %w", err)
			}
			v[i] = n
		}
		return CPUTimes{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}, nil
	}
	return CPUTimes{}, fmt.Errorf("parse /proc/stat: no cpu line")
}

// ParseMeminfo returns /proc/meminfo values in bytes, keyed by field name.
func ParseMeminfo(r io.Reader) (map[string]uint64, error) {
	out := map[string]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}
		out[key] = n
	}
	if _, ok := out["MemTotal"]; !ok {
		return nil, fmt.Errorf("parse /proc/meminfo: MemTotal missing")
	}
	return out, sc.Err()
}

func ParseLoadavg(s string) (l1, l5, l15 float64, err error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return 0, 0, 0, fmt.Errorf("parse /proc/loadavg: %q", s)
	}
	vals := make([]float64, 3)
	for i := range vals {
		if vals[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return 0, 0, 0, fmt.Errorf("parse /proc/loadavg: %w", err)
		}
	}
	return vals[0], vals[1], vals[2], nil
}

func ParseUptime(s string) (float64, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, fmt.Errorf("parse /proc/uptime: empty")
	}
	return strconv.ParseFloat(f[0], 64)
}

type NetCounters struct {
	RxBytes, RxErrors, TxBytes, TxErrors uint64
}

// ParseNetDev parses /proc/net/dev.
func ParseNetDev(r io.Reader) (map[string]NetCounters, error) {
	out := map[string]NetCounters{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // header lines
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		u := func(i int) uint64 { n, _ := strconv.ParseUint(f[i], 10, 64); return n }
		out[strings.TrimSpace(name)] = NetCounters{RxBytes: u(0), RxErrors: u(2), TxBytes: u(8), TxErrors: u(10)}
	}
	return out, sc.Err()
}

type Mount struct {
	Device, Point, FSType string
}

// pseudo filesystems that never represent real storage.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true, "securityfs": true,
	"cgroup": true, "cgroup2": true, "pstore": true, "bpf": true, "debugfs": true, "tracefs": true,
	"configfs": true, "fusectl": true, "mqueue": true, "hugetlbfs": true, "autofs": true, "binfmt_misc": true,
	"rpc_pipefs": true, "nsfs": true, "overlay": true, "squashfs": true, "efivarfs": true, "ramfs": true,
	"fuse.lxcfs": true, "fuse.gvfsd-fuse": true, "fuse.portal": true, "nfsd": true, "selinuxfs": true,
}

// ParseMounts parses /proc/self/mounts, keeping only real block-backed
// filesystems and one entry per device.
func ParseMounts(r io.Reader) ([]Mount, error) {
	var out []Mount
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		m := Mount{Device: unescapeMount(f[0]), Point: unescapeMount(f[1]), FSType: f[2]}
		if pseudoFS[m.FSType] {
			continue
		}
		if !strings.HasPrefix(m.Device, "/") && m.FSType != "zfs" && m.FSType != "btrfs" {
			continue
		}
		if seen[m.Device] {
			continue
		}
		seen[m.Device] = true
		out = append(out, m)
	}
	return out, sc.Err()
}

// unescapeMount decodes the octal escapes (\040 etc.) used in /proc/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ParseOSRelease parses /etc/os-release.
func ParseOSRelease(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		out[k] = strings.Trim(v, `"'`)
	}
	return out
}

func clampPct(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	}
	return v
}
