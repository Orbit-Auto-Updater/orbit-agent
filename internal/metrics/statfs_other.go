//go:build !linux

package metrics

import (
	"errors"

	"linux-dash-agent/internal/protocol"
)

// The agent only targets Linux; this stub lets the package build and its
// parsers be tested on other platforms.
func statDisk(Mount) (protocol.Disk, error) {
	return protocol.Disk{}, errors.New("statfs: unsupported platform")
}

func FreePercent(string) (float64, error) {
	return 0, errors.New("statfs: unsupported platform")
}
