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

const teardownTimeout = 2 * time.Minute

type Options struct {
	DryRun  bool
	Force   bool
	Trigger string // "cli" | "poll" | "webhook"
}

type Result struct {
	Changed      bool
	Skipped      bool
	SkipReason   string
	Applied      bool
	Reverted     bool
	Degraded     bool
	OldCommit    string
	NewCommit    string
	RolledBackTo string
	Services     []string
	Updated      []string
	Stacks       []string
	ChangedFiles []string
	HealthWatch  time.Duration
	Duration     time.Duration
	Err          error

	Notification    *notify.Embed
	NotificationKey string
}

type ComposeService interface {
	LoadProject(ctx context.Context, composeFiles []string, projectName string) (*types.Project, error)
	Up(ctx context.Context, project *types.Project) error
	Down(ctx context.Context, projectName string) error
}

type Locker interface {
	TryLock() (bool, error)
	Unlock() error
}

type Deps struct {
	Config   *config.Config
	Git      *git.Syncer
	Compose  ComposeService
	Health   health.Snapshotter
	Clock    health.Clock
	State    state.Store
	Notifier *notify.Notifier
	Lock     Locker
	Log      *slog.Logger
}

const (
	SkipInFlight = "reconcile already running in this process"
	SkipLocked   = "another composelock process holds the state lock"
)

var processSingleFlight sync.Mutex

var singleFlights sync.Map

func (d Deps) singleFlight() *sync.Mutex {
	if d.Config == nil || d.Config.StateFile == "" {
		return &processSingleFlight
	}
	mu, _ := singleFlights.LoadOrStore(d.Config.StateFile, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func Reconcile(ctx context.Context, opts Options, deps Deps) Result {
	start := time.Now()

	if !deps.singleFlight().TryLock() {
		deps.Log.Info("reconcile already running, skipped", "trigger", opts.Trigger)
		return Result{Skipped: true, SkipReason: SkipInFlight, Duration: time.Since(start)}
	}
	result := func() Result {
		defer deps.singleFlight().Unlock()

		if deps.Lock != nil {
			held, err := deps.Lock.TryLock()
			if err != nil {
				deps.Log.Error("acquiring state lock", "err", err)
				return Result{Err: fmt.Errorf("acquiring state lock: %w", err)}
			}
			if !held {
				deps.Log.Info("another composelock process holds the state lock, skipped", "trigger", opts.Trigger)
				return Result{Skipped: true, SkipReason: SkipLocked}
			}
			defer func() {
				if err := deps.Lock.Unlock(); err != nil {
					deps.Log.Error("releasing state lock", "err", err)
				}
			}()
		}

		return reconcileLocked(ctx, opts, deps, start)
	}()

	result.Duration = time.Since(start)
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

func healthOptions(cfg *config.Config, projectName, commit string, expectContainers bool) health.Options {
	return health.Options{
		ProjectName:          projectName,
		Commit:               commit,
		WatchDuration:        time.Duration(cfg.HealthWatchSeconds) * time.Second,
		PollInterval:         time.Duration(cfg.HealthPollIntervalSeconds) * time.Second,
		UnhealthyStreakLimit: cfg.HealthUnhealthyStreak,
		RestartTolerance:     cfg.HealthRestartTolerance,
		ExpectContainers:     expectContainers,
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
		if compose.IsHiddenPath(f) || !compose.IsComposeFile(filepath.Base(f)) {
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

func tearDownStacks(ctx context.Context, deps Deps, stacks []compose.Stack, reason string) error {
	if len(stacks) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
	defer cancel()

	var errs []error
	for _, s := range stacks {
		deps.Log.Info("tearing down compose stack", "project", s.ProjectName, "reason", reason)
		if err := deps.Compose.Down(ctx, s.ProjectName); err != nil {
			deps.Log.Error("tearing down compose stack failed", "project", s.ProjectName, "err", err)
			errs = append(errs, fmt.Errorf("tearing down %s: %w", s.ProjectName, err))
		}
	}
	return errors.Join(errs...)
}

type liveState struct {
	snapshots map[string]health.Snapshot
	healthy   bool
	reason    string
}

func evaluateLive(ctx context.Context, deps Deps, stacks []compose.Stack) (liveState, error) {
	names := stackNames(stacks)
	if len(names) == 0 {
		names = []string{deps.Config.ProjectName}
	}
	live := liveState{snapshots: make(map[string]health.Snapshot, len(names)), healthy: true}
	for _, name := range names {
		snap, err := deps.Health.Snapshot(ctx, name)
		if err != nil {
			return liveState{}, fmt.Errorf("snapshot of %s: %w", name, err)
		}
		live.snapshots[name] = snap
		if healthy, reason := health.EvaluatePreflight(snap); !healthy {
			live.healthy, live.reason = false, name+": "+reason
			return live, nil
		}
	}
	return live, nil
}

func updatedServices(cfg *config.Config, before, after map[string]health.Snapshot, stacks []compose.Stack) ([]string, bool) {
	if cfg.HealthWatchSeconds <= 0 {
		return nil, false
	}
	var updated []string
	for _, s := range stacks {
		post, ok := after[s.ProjectName]
		if !ok {
			return nil, false
		}
		updated = append(updated, health.ChangedServices(before[s.ProjectName], post)...)
	}
	slices.Sort(updated)
	return slices.Compact(updated), true
}

func recordOutcome(deps Deps, st *state.State, result state.Result, commit string) {
	st.LastResult = result
	st.LastAttemptAt = deps.Clock.Now()
	if commit != "" {
		st.LastAttemptCommit = commit
	}
	if err := deps.State.Save(st); err != nil {
		deps.Log.Error("saving state", "err", err, "last_result", string(result))
	}
}

func promoteHealthy(deps Deps, st *state.State, commit string) error {
	st.LastHealthyCommit = commit
	st.LastHealthyAt = deps.Clock.Now()
	st.PendingCommit = ""
	st.PendingSince = time.Time{}
	st.PendingAttempts = 0
	st.LastFailedCommit = ""
	st.LastFailedAt = time.Time{}
	st.LastAttemptCommit = commit
	st.LastAttemptAt = deps.Clock.Now()
	st.LastResult = state.ResultSuccess
	if err := deps.State.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}
