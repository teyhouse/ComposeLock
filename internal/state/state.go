package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const schemaVersion = 2

const maxFailedCommits = 16

const maxHistory = 20

type Result string

const (
	ResultSuccess         Result = "success"
	ResultFailedGit       Result = "failed_git"
	ResultFailedPreflight Result = "failed_preflight"
	ResultSkippedKnownBad Result = "skipped_known_bad"
	ResultFailedApply     Result = "failed_apply"
	ResultReverted        Result = "reverted"
	ResultDegraded        Result = "degraded"
)

type State struct {
	SchemaVersion int `json:"schema_version"`

	LastHealthyCommit string    `json:"last_healthy_commit"`
	LastHealthyAt     time.Time `json:"last_healthy_at,omitzero"`
	LastHealthyStacks []string  `json:"last_healthy_stacks,omitzero"`

	LastCheckoutCommit string `json:"last_checkout_commit,omitzero"`

	LastFailedCommit string    `json:"last_failed_commit"`
	LastFailedAt     time.Time `json:"last_failed_at,omitzero"`
	FailedCommits    []string  `json:"failed_commits,omitzero"`

	PreflightBlocks int `json:"preflight_blocks,omitzero"`

	LastResult Result `json:"last_result"`

	LastAttemptCommit string    `json:"last_attempt_commit"`
	LastAttemptAt     time.Time `json:"last_attempt_at,omitzero"`

	PendingCommit   string    `json:"pending_commit"`
	PendingSince    time.Time `json:"pending_since,omitzero"`
	PendingAttempts int       `json:"pending_attempts,omitzero"`
	PendingStacks   []string  `json:"pending_stacks,omitzero"`
	PendingRevert   bool      `json:"pending_revert,omitzero"`

	PausedAt    time.Time `json:"paused_at,omitzero"`
	PausedUntil time.Time `json:"paused_until,omitzero"`
	PauseReason string    `json:"pause_reason,omitzero"`

	History []Deployment `json:"history,omitzero"`
}

type Deployment struct {
	At           time.Time `json:"at"`
	Commit       string    `json:"commit,omitzero"`
	Result       Result    `json:"result"`
	RolledBackTo string    `json:"rolled_back_to,omitzero"`
	Error        string    `json:"error,omitzero"`
	Repeats      int       `json:"repeats,omitzero"`
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
	if st.SchemaVersion > schemaVersion {
		return nil, fmt.Errorf("state file %s has schema_version %d, this build understands %d: upgrade composelock", path, st.SchemaVersion, schemaVersion)
	}
	st.SchemaVersion = schemaVersion
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
	return s.PendingCommit != "" || s.PendingRevert
}

func (s *State) RevertInProgress() bool {
	return s.PendingRevert || (s.PendingCommit != "" && s.PendingCommit == s.LastFailedCommit)
}

func (s *State) IsKnownBad(commit string) bool {
	if commit == "" {
		return false
	}
	return commit == s.LastFailedCommit || slices.Contains(s.FailedCommits, commit)
}

func (s *State) MarkFailed(commit string, at time.Time) {
	s.LastFailedCommit = commit
	s.LastFailedAt = at
	if commit == "" {
		return
	}
	kept := slices.DeleteFunc(slices.Clone(s.FailedCommits), func(c string) bool { return c == commit })
	kept = append(kept, commit)
	if len(kept) > maxFailedCommits {
		kept = kept[len(kept)-maxFailedCommits:]
	}
	s.FailedCommits = kept
}

func (s *State) ForgetFailed(commit string) {
	if s.LastFailedCommit == commit {
		s.LastFailedCommit = ""
		s.LastFailedAt = time.Time{}
	}
	if !slices.Contains(s.FailedCommits, commit) {
		return
	}
	s.FailedCommits = slices.DeleteFunc(slices.Clone(s.FailedCommits), func(c string) bool { return c == commit })
	if len(s.FailedCommits) == 0 {
		s.FailedCommits = nil
	}
}

func (s *State) ForgetAllFailed() {
	s.LastFailedCommit = ""
	s.LastFailedAt = time.Time{}
	s.FailedCommits = nil
}

func (s *State) Paused(now time.Time) bool {
	return !s.PausedAt.IsZero() && (s.PausedUntil.IsZero() || now.Before(s.PausedUntil))
}

func (s *State) Pause(at, until time.Time, reason string) {
	s.PausedAt, s.PausedUntil, s.PauseReason = at, until, reason
}

func (s *State) Resume() {
	s.PausedAt, s.PausedUntil, s.PauseReason = time.Time{}, time.Time{}, ""
}

func (s *State) Record(d Deployment) {
	history := slices.Clone(s.History)
	if n := len(history); n > 0 && history[n-1].Commit == d.Commit && history[n-1].Result == d.Result {
		d.Repeats = history[n-1].Repeats + 1
		history[n-1] = d
	} else {
		history = append(history, d)
	}
	if len(history) > maxHistory {
		history = slices.Clone(history[len(history)-maxHistory:])
	}
	s.History = history
}

func (s *State) ExpectedCheckout() string {
	if s.LastCheckoutCommit != "" {
		return s.LastCheckoutCommit
	}
	return s.LastHealthyCommit
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
