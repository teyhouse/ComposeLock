package git

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teyhouse/ComposeLock/internal/execx"
)

type deadlineRunner struct {
	deadlines []time.Duration
	commands  []string
}

func (r *deadlineRunner) Run(ctx context.Context, _ string, _ []string, _ string, args ...string) ([]byte, []byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, nil, errors.New("no deadline set on the git command context")
	}
	r.deadlines = append(r.deadlines, time.Until(deadline))
	r.commands = append(r.commands, args[0])
	switch args[0] {
	case "rev-parse":
		return []byte("abc123"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestEveryGitCommandGetsADeadline(t *testing.T) {
	r := &deadlineRunner{}
	s := &Syncer{Runner: r, RepoPath: "/repo", Remote: "origin", Branch: "main"}

	if _, err := s.Preview(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.Checkout(t.Context(), "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]time.Duration{
		"fetch":     FetchTimeout,
		"rev-parse": QueryTimeout,
		"checkout":  CheckoutTimeout,
	}
	for i, cmd := range r.commands {
		limit, ok := want[cmd]
		if !ok {
			t.Fatalf("unexpected command %q", cmd)
		}
		if r.deadlines[i] > limit || r.deadlines[i] < limit/2 {
			t.Errorf("%s deadline = %v, want about %v", cmd, r.deadlines[i], limit)
		}
	}
}

type hangingRunner struct{}

func (hangingRunner) Run(ctx context.Context, _ string, _ []string, name string, args ...string) ([]byte, []byte, error) {
	return execx.OSRunner{}.Run(ctx, "", nil, "sh", "-c", "sleep 60")
}

func TestFetchDoesNotHangForeverOnAStuckRemote(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	s := &Syncer{Runner: hangingRunner{}, RepoPath: "/repo", Remote: "origin", Branch: "main"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Preview(ctx); err == nil {
			t.Error("expected an error from the stuck fetch")
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Preview never returned on a stuck remote")
	}
}
