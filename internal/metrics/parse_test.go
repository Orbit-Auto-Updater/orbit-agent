package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCPUUsage(t *testing.T) {
	a, err := ParseProcStat(strings.NewReader("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ParseProcStat(strings.NewReader("cpu  200 0 200 1400 200 0 0 0 0 0\n"))
	busy, iowait := CPUUsage(a, b)
	// delta total 1000, idle+iowait delta 800 → 20% busy, 10% iowait
	if busy != 20 || iowait != 10 {
		t.Fatalf("busy=%v iowait=%v", busy, iowait)
	}
	if busy, _ := CPUUsage(b, b); busy != 0 {
		t.Fatalf("no delta should be 0, got %v", busy)
	}
}

func TestParseMeminfo(t *testing.T) {
	mi, err := ParseMeminfo(strings.NewReader(`MemTotal:       16303428 kB
MemFree:          912345 kB
MemAvailable:    8151714 kB
SwapTotal:       2097148 kB
SwapFree:        1048574 kB
HugePages_Total:       0
`))
	if err != nil {
		t.Fatal(err)
	}
	if mi["MemTotal"] != 16303428*1024 || mi["MemAvailable"] != 8151714*1024 || mi["HugePages_Total"] != 0 {
		t.Fatalf("unexpected %v", mi)
	}
}

func TestParseNetDev(t *testing.T) {
	in := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1234567    1000    0    0    0     0          0         0  1234567    1000    0    0    0     0       0          0
  eth0: 98765432  80000    3    0    0     0          0         0 12345678   60000    1    0    0     0       0          0
`
	got, err := ParseNetDev(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	e := got["eth0"]
	if e.RxBytes != 98765432 || e.RxErrors != 3 || e.TxBytes != 12345678 || e.TxErrors != 1 {
		t.Fatalf("eth0 = %+v", e)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 interfaces, got %d", len(got))
	}
}

func TestParseMounts(t *testing.T) {
	in := `sysfs /sys sysfs rw,nosuid 0 0
proc /proc proc rw 0 0
/dev/nvme0n1p2 / ext4 rw,relatime 0 0
tmpfs /run tmpfs rw 0 0
/dev/nvme0n1p1 /boot/efi vfat rw 0 0
/dev/nvme0n1p2 /var/lib/docker/overlay ext4 rw 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
/dev/sdb1 /mnt/backup\040disk xfs rw 0 0
tank/data /tank/data zfs rw 0 0
`
	got, err := ParseMounts(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var points []string
	for _, m := range got {
		points = append(points, m.Point)
	}
	want := "/,/boot/efi,/mnt/backup disk,/tank/data"
	if strings.Join(points, ",") != want {
		t.Fatalf("got %v want %v", points, want)
	}
}

func TestParseOSRelease(t *testing.T) {
	osr := ParseOSRelease(strings.NewReader("NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\n# comment\n"))
	if osr["PRETTY_NAME"] != "Ubuntu 24.04.1 LTS" || osr["ID"] != "ubuntu" || osr["VERSION_ID"] != "24.04" {
		t.Fatalf("%v", osr)
	}
}

func TestCollectorFromFakeProc(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", "cpu  100 0 100 800 0 0 0 0 0 0\n")
	write("meminfo", "MemTotal: 1000 kB\nMemAvailable: 250 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	write("loadavg", "0.50 0.40 0.30 1/200 1234\n")
	write("uptime", "3600.5 7000.1\n")
	write("net/dev", "h\nh\n  eth0: 1000 0 0 0 0 0 0 0 500 0 0 0 0 0 0 0\n veth12: 1 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0\n")
	write("self/mounts", "")

	c := NewCollector([]string{"lo", "veth*"}, nil)
	c.ProcRoot = root
	m := c.Collect()
	if len(m.Errors) > 0 {
		t.Fatalf("errors: %v", m.Errors)
	}
	if m.CPUPercent != 0 {
		t.Fatalf("first sample should report 0 CPU, got %v", m.CPUPercent)
	}
	if m.MemPercent != 75 || m.Load1 != 0.5 || m.UptimeSeconds != 3600.5 {
		t.Fatalf("unexpected metrics %+v", m)
	}
	if len(m.Net) != 1 || m.Net[0].Name != "eth0" {
		t.Fatalf("veth should be excluded: %+v", m.Net)
	}

	write("stat", "cpu  150 0 150 900 0 0 0 0 0 0\n") // +100 busy, +100 idle
	m = c.Collect()
	if m.CPUPercent != 50 {
		t.Fatalf("want 50%% CPU, got %v", m.CPUPercent)
	}
}
