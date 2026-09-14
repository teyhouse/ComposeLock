package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/reconcile"
	"github.com/teyhouse/ComposeLock/internal/state"
)

func TestParseArgsAcceptsFlagsOnBothSidesOfTheCommand(t *testing.T) {
	f := newCLIFlags()

	command, err := parseArgs(f, []string{"--config", "composelock.json", "poll", "--force"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if command != "poll" {
		t.Errorf("command = %q, want %q", command, "poll")
	}
	if !f.force {
		t.Error("--force after the command was dropped")
	}
	if f.configPath != "composelock.json" {
		t.Errorf("configPath = %q, want the flag before the command to survive", f.configPath)
	}
	if err := checkFlagsFor(command, f); err == nil {
		t.Error("checkFlagsFor() = nil, want poll to reject --force wherever it was written")
	}
}

func TestParseArgsCarriesLateFlagsIntoOverrides(t *testing.T) {
	f := newCLIFlags()

	command, err := parseArgs(f, []string{"--config", "composelock.json", "sync", "--repo", "/srv/repo"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if command != "sync" {
		t.Errorf("command = %q, want %q", command, "sync")
	}
	o := f.overrides()
	if o.RepoPath == nil || *o.RepoPath != "/srv/repo" {
		t.Errorf("overrides().RepoPath = %v, want the flag written after the command to reach the config", o.RepoPath)
	}
}

func TestParseArgsHandlesEveryFlagPosition(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"flags after", []string{"init", "--force"}},
		{"flags before", []string{"--force", "init"}},
		{"flags on both sides", []string{"--log-format", "text", "init", "--force"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCLIFlags()
			command, err := parseArgs(f, tt.args)
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if command != "init" {
				t.Errorf("command = %q, want %q", command, "init")
			}
			if !f.force {
				t.Error("--force was dropped")
			}
		})
	}
}

func TestParseArgsWithoutACommandStaysEmpty(t *testing.T) {
	f := newCLIFlags()

	command, err := parseArgs(f, []string{"--version"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if command != "" {
		t.Errorf("command = %q, want no command so the caller can default it", command)
	}
	if !f.versionFlag {
		t.Error("--version was dropped")
	}
}

type recordingLock struct {
	held bool
}

func (l *recordingLock) TryLock() (bool, error) {
	l.held = true
	return true, nil
}

func (l *recordingLock) Unlock() error {
	l.held = false
	return nil
}

type lockWatchingSnapshotter struct {
	lock      *recordingLock
	heldAtRun bool
	calls     int
}

func (s *lockWatchingSnapshotter) Snapshot(context.Context, string) (health.Snapshot, error) {
	s.calls++
	if s.lock.held {
		s.heldAtRun = true
	}
	return health.Snapshot{}, nil
}

func TestStatusReleasesTheStateLockBeforeSnapshotting(t *testing.T) {
	lock := &recordingLock{}
	snap := &lockWatchingSnapshotter{lock: lock}
	cfg := &config.Config{
		RepoPath:    t.TempDir(),
		ComposeFile: "docker-compose.yml",
		ProjectName: "test-stack",
	}
	deps := reconcile.Deps{
		Config: cfg,
		Health: snap,
		State:  &state.MemStore{State: state.New()},
		Lock:   lock,
		Log:    slog.New(slog.DiscardHandler),
	}

	if code := cmdStatus(t.Context(), "composelock.json", cfg, deps); code != 0 {
		t.Fatalf("cmdStatus() = %d, want 0", code)
	}
	if snap.calls == 0 {
		t.Fatal("no snapshot was taken")
	}
	if snap.heldAtRun {
		t.Error("status held the state lock across the container snapshots, which makes a reconcile tick skip for as long as Docker is slow")
	}
	if lock.held {
		t.Error("status left the state lock held")
	}
}
