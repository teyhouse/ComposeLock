package git

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/teyhouse/ComposeLock/internal/execx"
)

const (
	FetchTimeout    = 2 * time.Minute
	QueryTimeout    = 30 * time.Second
	CheckoutTimeout = 60 * time.Second
)

type Syncer struct {
	Runner   execx.Runner
	RepoPath string
	Remote   string
	Branch   string
	SSHKey   string
}

type Result struct {
	Changed      bool
	OldCommit    string
	NewCommit    string
	ChangedFiles []string
}

func (s *Syncer) env() []string {
	if s.SSHKey == "" {
		return nil
	}
	return []string{"GIT_SSH_COMMAND=ssh -i " + shellQuote(s.SSHKey) + " -o IdentitiesOnly=yes"}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (s *Syncer) git(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, _, err := s.Runner.Run(ctx, s.RepoPath, s.env(), "git", args...)
	return strings.TrimSpace(string(stdout)), err
}

func (s *Syncer) Preview(ctx context.Context) (Result, error) {
	if _, err := s.git(ctx, FetchTimeout, "fetch", s.Remote, s.Branch); err != nil {
		return Result{}, fmt.Errorf("git fetch: %w", err)
	}

	oldCommit, err := s.git(ctx, QueryTimeout, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("git rev-parse HEAD: %w", err)
	}

	newCommit, err := s.git(ctx, QueryTimeout, "rev-parse", s.Remote+"/"+s.Branch)
	if err != nil {
		return Result{}, fmt.Errorf("git rev-parse %s/%s: %w", s.Remote, s.Branch, err)
	}

	if oldCommit == newCommit {
		return Result{Changed: false, OldCommit: oldCommit, NewCommit: newCommit}, nil
	}

	changedOut, err := s.git(ctx, QueryTimeout, "diff", "--name-only", oldCommit, newCommit)
	if err != nil {
		return Result{}, fmt.Errorf("git diff: %w", err)
	}
	var changedFiles []string
	if changedOut != "" {
		changedFiles = strings.Split(changedOut, "\n")
	}

	return Result{
		Changed:      true,
		OldCommit:    oldCommit,
		NewCommit:    newCommit,
		ChangedFiles: changedFiles,
	}, nil
}

func (s *Syncer) Checkout(ctx context.Context, commit string) error {
	if _, err := s.git(ctx, CheckoutTimeout, "checkout", commit); err != nil {
		return fmt.Errorf("git checkout %s: %w", commit, err)
	}
	return nil
}

func Fetch(ctx context.Context, s *Syncer, attempts int, delay time.Duration, log *slog.Logger) (Result, error) {
	return retry(ctx, attempts, delay, log, s.Preview)
}

func retry(ctx context.Context, attempts int, delay time.Duration, log *slog.Logger, fn func(context.Context) (Result, error)) (Result, error) {
	attempts = max(attempts, 1)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		result, err := fn(ctx)
		if err == nil {
			return result, nil
		}
		lastErr = err
		log.Warn("git sync attempt failed", "attempt", attempt, "err", err)
		if attempt < attempts {
			if !sleepCtx(ctx, delay) {
				return Result{}, ctx.Err()
			}
		}
	}
	return Result{}, fmt.Errorf("git sync failed after %d attempts: %w", attempts, lastErr)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
