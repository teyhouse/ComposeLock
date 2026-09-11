package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const schemaVersion = 1

type Result string

const (
	ResultSuccess     Result = "success"
	ResultFailedApply Result = "failed_apply"
	ResultReverted    Result = "reverted"
	ResultDegraded    Result = "degraded"
)

type State struct {
	SchemaVersion int `json:"schema_version"`

	LastHealthyCommit string    `json:"last_healthy_commit"`
	LastHealthyAt     time.Time `json:"last_healthy_at,omitzero"`

	LastFailedCommit string    `json:"last_failed_commit"`
	LastFailedAt     time.Time `json:"last_failed_at,omitzero"`

	LastResult Result `json:"last_result"`

	LastAttemptCommit string    `json:"last_attempt_commit"`
	LastAttemptAt     time.Time `json:"last_attempt_at,omitzero"`

	PendingCommit string    `json:"pending_commit"`
	PendingSince  time.Time `json:"pending_since,omitzero"`
}

func New() *State {
	return &State{SchemaVersion: schemaVersion}
}

func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state file %s: %w", path, err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing state file %s: %w", path, err)
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = schemaVersion
	}
	return &st, nil
}

// Save is atomic: write a temp file in the same directory, fsync, rename over the destination.
func Save(path string, st *State) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		tmp.Close()
		return fmt.Errorf("marshaling state: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming temp state file into place: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("fsync state directory: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func CheckWritable(path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	return errors.Join(tmp.Close(), os.Remove(name))
}

func (s *State) Pending() bool {
	return s.PendingCommit != ""
}

func (s *State) RevertInProgress() bool {
	return s.Pending() && s.PendingCommit == s.LastFailedCommit
}

type Store interface {
	Load() (*State, error)
	Save(*State) error
}

type FileStore struct {
	Path string
}

func (f FileStore) Load() (*State, error) { return Load(f.Path) }
func (f FileStore) Save(st *State) error  { return Save(f.Path, st) }

type MemStore struct {
	State *State
}

func (m *MemStore) Load() (*State, error) {
	if m.State == nil {
		return New(), nil
	}
	st := *m.State
	return &st, nil
}

func (m *MemStore) Save(st *State) error {
	saved := *st
	m.State = &saved
	return nil
}
