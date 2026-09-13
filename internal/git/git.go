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

func (s *Syncer) fetch(ctx context.Context) error {
	if _, err := s.git(ctx, FetchTimeout, "fetch", s.Remote, s.Branch); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	return nil
}

func (s *Syncer) Preview(ctx context.Context) (Result, error) {
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

	changedOut, err := s.git(ctx, QueryTimeout, "diff", "--name-only", "-z", oldCommit, newCommit)
	if err != nil {
		return Result{}, fmt.Errorf("git diff: %w", err)
	}
	changedFiles := splitNUL(changedOut)

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

func splitNUL(out string) []string {
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

func Fetch(ctx context.Context, s *Syncer, attempts int, delay time.Duration, log *slog.Logger) (Result, error) {
	if err := retry(ctx, attempts, delay, log, s.fetch); err != nil {
		return Result{}, err
	}
	return s.Preview(ctx)
}

func retry(ctx context.Context, attempts int, delay time.Duration, log *slog.Logger, fn func(context.Context) error) error {
	attempts = max(attempts, 1)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		log.Warn("git sync attempt failed", "attempt", attempt, "err", err)
		if permanent(err) {
			log.Warn("git sync: error will not be fixed by retrying, giving up", "err", err)
			break
		}
		if attempt < attempts {
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("git sync failed: %w", lastErr)
}

var permanentFetchErrors = []string{
	"not a git repository",
	"does not appear to be a git repository",
	"couldn't find remote ref",
	"repository not found",
	"invalid refspec",
}

func permanent(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range permanentFetchErrors {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
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
