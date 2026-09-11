package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	t         *testing.T
	responses map[string]fakeResponse
	calls     []string
}

type fakeResponse struct {
	stdout string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, []byte, error) {
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	resp, ok := f.responses[key]
	if !ok {
		f.t.Fatalf("unexpected command: %s", key)
	}
	return []byte(resp.stdout), nil, resp.err
}

func newSyncer(t *testing.T, responses map[string]fakeResponse) (*Syncer, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{t: t, responses: responses}
	return &Syncer{
		Runner:   fr,
		RepoPath: "/repo",
		Remote:   "origin",
		Branch:   "main",
	}, fr
}

func TestSyncOnceNoChange(t *testing.T) {
	s, fr := newSyncer(t, map[string]fakeResponse{
		"git fetch origin main":     {},
		"git rev-parse HEAD":        {stdout: "abc123"},
		"git rev-parse origin/main": {stdout: "abc123"},
	})

	result, err := s.SyncOnce(t.Context())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if result.Changed {
		t.Error("expected Changed = false")
	}
	for _, call := range fr.calls {
		if strings.Contains(call, "checkout") {
			t.Errorf("unexpected checkout call on no-change path: %s", call)
		}
	}
}

func TestSyncOnceChange(t *testing.T) {
	s, _ := newSyncer(t, map[string]fakeResponse{
		"git fetch origin main":              {},
		"git rev-parse HEAD":                 {stdout: "old111"},
		"git rev-parse origin/main":          {stdout: "new222"},
		"git diff --name-only old111 new222": {stdout: "docker-compose.yml\napp.env.example"},
		"git checkout new222":                {},
	})

	result, err := s.SyncOnce(t.Context())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed = true")
	}
	if result.OldCommit != "old111" || result.NewCommit != "new222" {
		t.Errorf("commits = %q -> %q, want old111 -> new222", result.OldCommit, result.NewCommit)
	}
	wantFiles := []string{"docker-compose.yml", "app.env.example"}
	if len(result.ChangedFiles) != len(wantFiles) {
		t.Fatalf("ChangedFiles = %v, want %v", result.ChangedFiles, wantFiles)
	}
	for i, f := range wantFiles {
		if result.ChangedFiles[i] != f {
			t.Errorf("ChangedFiles[%d] = %q, want %q", i, result.ChangedFiles[i], f)
		}
	}
}

func TestSyncRetriesExhausted(t *testing.T) {
	fetchErr := errors.New("network unreachable")
	fr := &fakeRunner{t: t, responses: map[string]fakeResponse{
		"git fetch origin main": {err: fetchErr},
	}}
	s := &Syncer{Runner: fr, RepoPath: "/repo", Remote: "origin", Branch: "main"}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	_, err := Sync(t.Context(), s, 3, time.Millisecond, log)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	fetchCalls := 0
	for _, c := range fr.calls {
		if c == "git fetch origin main" {
			fetchCalls++
		}
	}
	if fetchCalls != 3 {
		t.Errorf("fetch called %d times, want 3", fetchCalls)
	}
}

func TestSyncFailsTwiceThenSucceeds(t *testing.T) {
	attempt := 0
	responses := map[string]fakeResponse{
		"git rev-parse HEAD":        {stdout: "abc123"},
		"git rev-parse origin/main": {stdout: "abc123"},
	}
	fr := &fakeRunner{t: t, responses: responses}
	countingRunner := runnerFunc(func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
		key := name + " " + strings.Join(args, " ")
		if key == "git fetch origin main" {
			attempt++
			if attempt < 3 {
				return nil, nil, fmt.Errorf("flaky attempt %d", attempt)
			}
			return nil, nil, nil
		}
		return fr.Run(ctx, dir, env, name, args...)
	})

	s := &Syncer{Runner: countingRunner, RepoPath: "/repo", Remote: "origin", Branch: "main"}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	result, err := Sync(t.Context(), s, 3, time.Millisecond, log)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Changed {
		t.Error("expected Changed = false")
	}
	if attempt != 3 {
		t.Errorf("fetch attempted %d times, want 3 (2 failures + 1 success)", attempt)
	}
}

type runnerFunc func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, []byte, error)

func (f runnerFunc) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
	return f(ctx, dir, env, name, args...)
}
