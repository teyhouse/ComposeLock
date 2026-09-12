package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/compose"
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
	Stacks       []string
	ChangedFiles []string
	HealthWatch  time.Duration
	Duration     time.Duration
	Err          error

	Notification *notify.Embed
}

type ComposeService interface {
	LoadProject(ctx context.Context, composeFiles []string, projectName string) (*types.Project, error)
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

func healthOptions(cfg *config.Config, projectName, commit string) health.Options {
	return health.Options{
		ProjectName:          projectName,
		Commit:               commit,
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

func StacksFor(cfg *config.Config) ([]compose.Stack, error) {
	if cfg.ComposeDir != "" {
		return compose.Discover(resolveUnderRepo(cfg.RepoPath, cfg.ComposeDir), cfg.ProjectName)
	}
	return []compose.Stack{{ProjectName: cfg.ProjectName, Files: []string{cfg.ComposeFile}}}, nil
}

func stackNames(stacks []compose.Stack) []string {
	names := make([]string, len(stacks))
	for i, s := range stacks {
		names[i] = s.ProjectName
	}
	return names
}

func reportStacks(cfg *config.Config, names []string) []string {
	if cfg.ComposeDir == "" {
		return nil
	}
	return names
}

func filterChanged(repoPath string, stacks []compose.Stack, changedFiles []string) []compose.Stack {
	changedDirs := changedComposeDirs(repoPath, changedFiles)
	var changed []compose.Stack
	for _, s := range stacks {
		if s.Dir == "" {
			if stackFilesChanged(repoPath, s.Files, changedFiles) {
				changed = append(changed, s)
			}
			continue
		}
		if _, ok := changedDirs[resolveUnderRepo(repoPath, s.Dir)]; ok {
			changed = append(changed, s)
		}
	}
	return changed
}

func changedComposeDirs(repoPath string, changedFiles []string) map[string]struct{} {
	dirs := make(map[string]struct{}, len(changedFiles))
	for _, f := range changedFiles {
		if !compose.IsComposeFile(filepath.Base(f)) {
			continue
		}
		dirs[filepath.Dir(resolveUnderRepo(repoPath, f))] = struct{}{}
	}
	return dirs
}

func intersectByProjectName(a, b []compose.Stack) []compose.Stack {
	names := make(map[string]struct{}, len(b))
	for _, s := range b {
		names[s.ProjectName] = struct{}{}
	}
	var out []compose.Stack
	for _, s := range a {
		if _, ok := names[s.ProjectName]; ok {
			out = append(out, s)
		}
	}
	return out
}

func vanishedStacks(previous, current []compose.Stack) []compose.Stack {
	currentNames := make(map[string]struct{}, len(current))
	for _, s := range current {
		currentNames[s.ProjectName] = struct{}{}
	}
	var gone []compose.Stack
	for _, s := range previous {
		if _, ok := currentNames[s.ProjectName]; !ok {
			gone = append(gone, s)
		}
	}
	return gone
}

func tearDownStacks(ctx context.Context, deps Deps, stacks []compose.Stack, reason string) {
	for _, s := range stacks {
		deps.Log.Info("tearing down compose stack", "project", s.ProjectName, "reason", reason)
		if err := deps.Compose.Down(ctx, s.ProjectName); err != nil {
			deps.Log.Error("tearing down compose stack failed", "project", s.ProjectName, "err", err)
		}
	}
}

func evaluateLive(ctx context.Context, deps Deps, stacks []compose.Stack) (bool, string, error) {
	names := stackNames(stacks)
	if len(names) == 0 {
		names = []string{deps.Config.ProjectName}
	}
	for _, name := range names {
		snap, err := deps.Health.Snapshot(ctx, name)
		if err != nil {
			return false, "", fmt.Errorf("snapshot of %s: %w", name, err)
		}
		if healthy, reason := health.Evaluate(snap); !healthy {
			return false, name + ": " + reason, nil
		}
	}
	return true, "", nil
}

func promoteHealthy(deps Deps, st *state.State, commit string) error {
	st.LastHealthyCommit = commit
	st.LastHealthyAt = deps.Clock.Now()
	st.PendingCommit = ""
	st.PendingSince = time.Time{}
	st.LastFailedCommit = ""
	st.LastFailedAt = time.Time{}
	st.LastResult = state.ResultSuccess
	if err := deps.State.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}
