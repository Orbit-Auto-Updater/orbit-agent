// Package state persists agent credentials and progress markers across
// restarts in a single JSON file written atomically with 0600 permissions.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"linux-dash-agent/internal/protocol"
)

type State struct {
	AgentID          string              `json:"agent_id,omitempty"`
	Secret           string              `json:"secret,omitempty"`
	ServerURL        string              `json:"server_url,omitempty"`
	JournalCursor    string              `json:"journal_cursor,omitempty"`
	RemotePolicy     *protocol.Policy    `json:"remote_policy,omitempty"`
	LastUpdateWindow time.Time           `json:"last_update_window,omitzero"`
	LastUpdateRun    *protocol.UpdateRun `json:"last_update_run,omitempty"`
	// PendingUpdate is set while a self-update awaits confirmation.
	PendingUpdate  *PendingUpdate              `json:"pending_update,omitempty"`
	LastSelfUpdate *protocol.SelfUpdateOutcome `json:"last_self_update,omitempty"`
}

type PendingUpdate struct {
	From           string    `json:"from"`
	To             string    `json:"to"`
	Backup         string    `json:"backup"` // path of the previous binary
	Binary         string    `json:"binary"`
	At             time.Time `json:"at"`
	Failures       int       `json:"failures"`        // startups without reporting in
	ReportFailures int       `json:"report_failures"` // failed reports by the new version
}

type Store struct {
	path string
	mu   sync.Mutex
	s    State
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st := &Store{path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &st.s); err != nil {
		return nil, err
	}
	return st, nil
}

// Get returns a copy of the current state.
func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s
}

// Update mutates the state and saves it.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.s
	fn(&next)
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.s = next
	return nil
}
