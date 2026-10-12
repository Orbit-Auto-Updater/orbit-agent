// Package journal reads new error-level entries from systemd-journald.
package journal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"linux-dash-agent/internal/execx"
	"linux-dash-agent/internal/protocol"
)

type Reader struct {
	Priority   string // journalctl -p value, e.g. "err" or "warning"
	MaxEntries int
	Runner     execx.Runner
}

// Read returns entries after cursor (or from the last 10 minutes when cursor
// is empty) and the cursor to resume from next time.
func (r *Reader) Read(ctx context.Context, cursor string) ([]protocol.LogEntry, string, error) {
	max := r.MaxEntries
	if max <= 0 {
		max = 200
	}
	prio := r.Priority
	if prio == "" {
		prio = "err"
	}
	args := []string{"--no-pager", "-o", "json", "-p", prio, "-n", strconv.Itoa(max)}
	if cursor != "" {
		args = append(args, "--after-cursor="+cursor)
	} else {
		args = append(args, "--since=-10min")
	}
	res, err := r.Runner.Run(ctx, execx.Cmd{Name: "journalctl", Args: args, Timeout: 30 * time.Second})
	if err != nil {
		return nil, cursor, err
	}
	if res.ExitCode != 0 {
		return nil, cursor, fmt.Errorf("journalctl exited %d: %s", res.ExitCode, execx.Tail(string(res.Stderr), 300))
	}
	entries, next := Parse(res.Stdout)
	if next == "" {
		next = cursor
	}
	return entries, next, nil
}

// Parse decodes `journalctl -o json` output (one object per line).
func Parse(out []byte) ([]protocol.LogEntry, string) {
	var entries []protocol.LogEntry
	var cursor string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		if c := str(raw["__CURSOR"]); c != "" {
			cursor = c
		}
		msg := str(raw["MESSAGE"])
		if msg == "" {
			continue
		}
		e := protocol.LogEntry{Message: truncate(msg, 2000)}
		if us, err := strconv.ParseInt(str(raw["__REALTIME_TIMESTAMP"]), 10, 64); err == nil {
			e.Time = time.UnixMicro(us).UTC()
		}
		e.Priority, _ = strconv.Atoi(str(raw["PRIORITY"]))
		e.Unit = str(raw["_SYSTEMD_UNIT"])
		if e.Unit == "" {
			e.Unit = str(raw["SYSLOG_IDENTIFIER"])
		}
		entries = append(entries, e)
	}
	return entries, cursor
}

// str decodes a journal field that is usually a string but is an array of
// bytes when the value is not valid UTF-8.
func str(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b []byte
	var ints []int
	if json.Unmarshal(raw, &ints) == nil {
		for _, i := range ints {
			b = append(b, byte(i))
		}
		return strings.ToValidUTF8(string(b), "�")
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
