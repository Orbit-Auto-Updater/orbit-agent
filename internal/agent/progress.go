package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"linux-dash-agent/internal/packages"
	"linux-dash-agent/internal/protocol"
	"linux-dash-agent/internal/transport"
	"linux-dash-agent/internal/updater"
)

const (
	flushEvery     = time.Second
	maxLineLen     = 2000
	maxJobLines    = 20000 // per run; beyond this only a notice is sent
	maxQueuedLines = 5000  // unsent backlog while the dashboard is unreachable
)

// jobStream sends an update run's output, phase and installed packages to the
// dashboard about once a second while it runs.
type jobStream struct {
	client  *transport.Client
	log     interface{ Warn(string, ...any) }
	manager string
	base    protocol.Progress

	mu        sync.Mutex
	nextSeq   int // sequence number of the next line
	sentSeq   int // first line not yet acknowledged
	queue     []protocol.LogLine
	installed []protocol.InstalledPackage
	seen      map[string]bool
	phase     string
	total     int
	truncated bool
	final     *protocol.Progress
	disabled  bool

	stop chan struct{}
	done chan struct{}
}

func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "run_" + hex.EncodeToString(b)
}

func (a *Agent) newJobStream(trigger, commandID, manager string) updater.Reporter {
	return a.newStream(protocol.CmdUpdatesApply, trigger, commandID, manager)
}

func (a *Agent) newStream(kind, trigger, commandID, manager string) *jobStream {
	s := &jobStream{
		client: a.client, log: a.log, manager: manager, seen: map[string]bool{},
		base: protocol.Progress{RunID: newRunID(), CommandID: commandID, Kind: kind, Trigger: trigger, StartedAt: time.Now().UTC()},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *jobStream) Phase(p string) {
	s.mu.Lock()
	s.phase = p
	s.mu.Unlock()
}

func (s *jobStream) Total(n int) {
	s.mu.Lock()
	s.total = n
	s.mu.Unlock()
}

func (s *jobStream) Line(stream, text string) {
	if len(text) > maxLineLen {
		text = text[:maxLineLen] + "…"
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if pkg, ok := packages.ParseInstalled(s.manager, text); ok && !s.seen[pkg.Name] {
		s.seen[pkg.Name] = true
		s.installed = append(s.installed, protocol.InstalledPackage{Name: pkg.Name, Version: pkg.Version, Time: now})
	}
	if s.nextSeq >= maxJobLines {
		if !s.truncated {
			s.truncated = true
			s.queue = append(s.queue, protocol.LogLine{Time: now, Stream: "agent", Text: "… output truncated (log limit reached)"})
			s.nextSeq++
		}
		return
	}
	s.queue = append(s.queue, protocol.LogLine{Time: now, Stream: stream, Text: text})
	s.nextSeq++
	if over := len(s.queue) - maxQueuedLines; over > 0 { // dashboard unreachable for a while
		s.queue = s.queue[over:]
		s.sentSeq += over
	}
}

func (s *jobStream) Finish(run protocol.UpdateRun) {
	s.mu.Lock()
	s.final = &protocol.Progress{Done: true, Status: run.Status, Error: run.Error}
	s.mu.Unlock()
	close(s.stop)
	<-s.done
}

func (s *jobStream) loop() {
	defer close(s.done)
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.flush()
		case <-s.stop:
			// Deliver the tail and the final status, retrying briefly.
			for i := 0; i < 5; i++ {
				if s.flush() {
					return
				}
				time.Sleep(time.Duration(i+1) * time.Second)
			}
			return
		}
	}
}

// flush sends everything queued; it returns true once the final chunk is delivered.
func (s *jobStream) flush() bool {
	s.mu.Lock()
	if s.disabled {
		s.mu.Unlock()
		return true
	}
	p := s.base
	p.Phase, p.Total, p.Seq = s.phase, s.total, s.sentSeq
	p.Lines = append([]protocol.LogLine(nil), s.queue...)
	p.Installed = append([]protocol.InstalledPackage(nil), s.installed...)
	final := s.final
	if final != nil {
		p.Done, p.Status, p.Error = true, final.Status, final.Error
	}
	s.mu.Unlock()

	if len(p.Lines) == 0 && len(p.Installed) == 0 && final == nil && p.Phase == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := s.client.Progress(ctx, &p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, transport.ErrNotSupported) {
		s.disabled = true // older dashboard: the command result still carries the output
		return true
	}
	if err != nil {
		s.log.Warn("sending job progress failed; will retry", "run", p.RunID, "err", err)
		return false
	}
	// Drop what was acknowledged, by sequence number: lines may have been
	// added (or the backlog trimmed) while the request was in flight.
	if drop := min(p.Seq+len(p.Lines)-s.sentSeq, len(s.queue)); drop > 0 {
		s.queue = s.queue[drop:]
		s.sentSeq += drop
	}
	s.installed = s.installed[min(len(p.Installed), len(s.installed)):]
	return final != nil
}
