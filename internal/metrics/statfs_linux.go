//go:build linux

package metrics

import (
	"syscall"

	"orbit-agent/internal/protocol"
)

func statDisk(m Mount) (protocol.Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.Point, &st); err != nil {
		return protocol.Disk{}, err
	}
	bs := uint64(st.Bsize)
	total := st.Blocks * bs
	free := st.Bavail * bs // available to unprivileged users, as df reports
	used := (st.Blocks - st.Bfree) * bs
	d := protocol.Disk{Mount: m.Point, Device: m.Device, FSType: m.FSType, TotalBytes: total, UsedBytes: used, FreeBytes: free}
	if used+free > 0 {
		d.UsedPercent = round1(float64(used) / float64(used+free) * 100)
	}
	if st.Files > 0 {
		d.InodesPercent = round1(float64(st.Files-st.Ffree) / float64(st.Files) * 100)
	}
	return d, nil
}

// FreePercent returns the share of space available on the filesystem holding path.
func FreePercent(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	used := st.Blocks - st.Bfree
	if used+st.Bavail == 0 {
		return 100, nil
	}
	return float64(st.Bavail) / float64(used+st.Bavail) * 100, nil
}
