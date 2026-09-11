package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

var errDegraded = errors.New("system is DEGRADED; use --force to retry")

type Options struct {
	DryRun  bool
	Force   bool
	Trigger string // "cli" | "poll" | "webhook"
}

type Result struct {
	Changed      bool
	Skipped      bool
	Applied      bool
	Reverted     bool
	Degraded     bool
	OldCommit    string
	NewCommit    string
	RolledBackTo string
	Services     []string
	ChangedFiles []string
	HealthWatch  time.Duration
	Duration     time.Duration
	Err          error

	Notification *notify.Embed
}

type ComposeService interface {
	LoadProject(ctx context.Context, composeFile, projectName string) (*types.Project, error)
	Up(ctx context.Context, project *types.Project) error
	Down(ctx context.Context, projectName string) error
}

type Deps struct {
	Config   *config.Config
	Git      *git.Syncer
	Compose  ComposeService
	Health   health.Snapshotter
	Clock    health.Clock
	State    state.Store
	Notifier *notify.Notifier
	Log      *slog.Logger
}

var singleFlight sync.Mutex

func Reconcile(ctx context.Context, opts Options, deps Deps) Result {
	start := time.Now()

	if !singleFlight.TryLock() {
		deps.Log.Info("reconcile already running, skipped", "trigger", opts.Trigger)
		return Result{Skipped: true, Duration: time.Since(start)}
	}
	result := func() Result {
		defer singleFlight.Unlock()
		return reconcileLocked(ctx, opts, deps, start)
	}()

	result.Duration = time.Since(start)

	if result.Notification != nil {
		deps.Notifier.Send(context.WithoutCancel(ctx), *result.Notification)
	}
	return result
}

func reconcileLocked(ctx context.Context, opts Options, deps Deps, start time.Time) Result {
	st, err := deps.State.Load()
	if err != nil {
		deps.Log.Error("loading state", "err", err)
		return Result{Err: fmt.Errorf("loading state: %w", err)}
	}

	if st.LastResult == state.ResultDegraded && !opts.Force {
		deps.Log.Warn("refusing to reconcile: system is DEGRADED, use --force", "pending_commit", st.PendingCommit)
		return Result{Degraded: true, NewCommit: st.PendingCommit, Err: errDegraded}
	}

	if st.Pending() {
		return recoverFromCrash(ctx, opts, deps, st, start)
	}
	return runNormal(ctx, opts, deps, st, start)
}

func healthOptions(cfg *config.Config) health.Options {
	return health.Options{
		ProjectName:          cfg.ProjectName,
		WatchDuration:        time.Duration(cfg.HealthWatchSeconds) * time.Second,
		PollInterval:         time.Duration(cfg.HealthPollIntervalSeconds) * time.Second,
		UnhealthyStreakLimit: cfg.HealthUnhealthyStreak,
		RestartTolerance:     cfg.HealthRestartTolerance,
	}
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

func serviceNames(project *types.Project) []string {
	return slices.Sorted(maps.Keys(project.Services))
}
