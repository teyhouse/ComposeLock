package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/heartbeat"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

var errDegraded = errors.New("system is DEGRADED; use --force to retry")

const teardownTimeout = 2 * time.Minute

const maxConcurrentTeardowns = 8

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
	Restarted    []string
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
	Config    *config.Config
	Git       *git.Syncer
	Compose   ComposeService
	Health    health.Snapshotter
	Clock     health.Clock
	State     state.Store
	Notifier  *notify.Notifier
	Heartbeat *heartbeat.Pinger
	Lock      Locker
	Log       *slog.Logger
}

const (
	SkipInFlight           = "reconcile already running in this process"
	SkipLocked             = "another composelock process holds the state lock"
	SkipPreflightUnhealthy = "pre-flight gate would block this commit: the live stack is unhealthy"
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

var activities sync.Map

func (d Deps) activity() *atomic.Uint64 {
	key := ""
	if d.Config != nil {
		key = d.Config.StateFile
	}
	a, _ := activities.LoadOrStore(key, new(atomic.Uint64))
	return a.(*atomic.Uint64)
}

func Reconcile(ctx context.Context, opts Options, deps Deps) Result {
	start := time.Now()

	if !deps.singleFlight().TryLock() {
		deps.Log.Info("reconcile already running, skipped", "trigger", opts.Trigger)
		return Result{Skipped: true, SkipReason: SkipInFlight, Duration: time.Since(start)}
	}
	deps.activity().Add(1)
	result := func() Result {
		defer func() {
			deps.activity().Add(1)
			deps.singleFlight().Unlock()
		}()

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

	if result.Notification != nil {
		addCommitContext(ctx, deps, result.Notification, result.OldCommit, result.RolledBackTo)
	}
	if !opts.DryRun && result.SkipReason != SkipInFlight && result.SkipReason != SkipLocked {
		deps.Heartbeat.Ping(ctx)
	}
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
		deps.Log.Warn("refusing to reconcile: system is DEGRADED, use --force", "commit", st.LastAttemptCommit)
		return Result{Degraded: true, NewCommit: st.LastAttemptCommit, Err: errDegraded}
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

func StacksFor(cfg *config.Config, log *slog.Logger) ([]compose.Stack, error) {
	if cfg.ComposeDir != "" {
		return compose.Discover(resolveUnderRepo(cfg.RepoPath, cfg.ComposeDir), cfg.ProjectName, log)
	}
	return []compose.Stack{{ProjectName: cfg.ProjectName, Files: []string{resolveUnderRepo(cfg.RepoPath, cfg.ComposeFile)}}}, nil
}

func stacksAllowingEmpty(deps Deps) ([]compose.Stack, error) {
	stacks, err := StacksFor(deps.Config, deps.Log)
	if errors.Is(err, compose.ErrNoStacks) {
		deps.Log.Info("no compose stacks at this checkout", "err", err)
		return nil, nil
	}
	return stacks, err
}

func deployedStacks(st *state.State, discovered []compose.Stack) []compose.Stack {
	if len(st.LastHealthyStacks) == 0 {
		return discovered
	}
	seen := make(map[string]struct{}, len(discovered)+len(st.LastHealthyStacks))
	out := slices.Clone(discovered)
	for _, s := range discovered {
		seen[s.ProjectName] = struct{}{}
	}
	for _, name := range st.LastHealthyStacks {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, compose.Stack{ProjectName: name})
	}
	return out
}

func restrictToNames(stacks []compose.Stack, names []string) []compose.Stack {
	if len(names) == 0 {
		return stacks
	}
	found := make(map[string]compose.Stack, len(stacks))
	for _, s := range stacks {
		found[s.ProjectName] = s
	}
	out := make([]compose.Stack, 0, len(names))
	for _, n := range names {
		if s, ok := found[n]; ok {
			out = append(out, s)
			continue
		}
		out = append(out, compose.Stack{ProjectName: n})
	}
	return out
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

func filterChanged(ctx context.Context, deps Deps, stacks, previous []compose.Stack, changedFiles []string) (changed []compose.Stack, undetermined bool) {
	repoPath := deps.Config.RepoPath
	touched := touchedStackDirs(repoPath, append(slices.Clone(stacks), previous...), changedFiles)
	for _, s := range stacks {
		if s.Dir == "" {
			changed = append(changed, s)
			continue
		}
		if _, ok := touched[resolveUnderRepo(repoPath, s.Dir)]; ok {
			changed = append(changed, s)
			continue
		}
		match, determined := stackInputsMatch(ctx, deps, s, changedFiles)
		if !determined {
			undetermined = true
			continue
		}
		if match {
			deps.Log.Info("a changed file outside compose_dir is an input of this stack", "project", s.ProjectName)
			changed = append(changed, s)
		}
	}
	return changed, undetermined
}

func stackInputsMatch(ctx context.Context, deps Deps, s compose.Stack, changedFiles []string) (match, determined bool) {
	project, err := deps.Compose.LoadProject(ctx, s.Files, s.ProjectName)
	if err != nil {
		deps.Log.Warn("loading a compose project to attribute a change failed, leaving the stack out of this cycle",
			"project", s.ProjectName, "err", err)
		return false, false
	}
	inputs, complete := compose.ProjectInputs(project)
	if !complete {
		deps.Log.Info("compose project has inputs that cannot be resolved without applying it, treating the stack as changed",
			"project", s.ProjectName, "inputs", inputs)
		return true, true
	}
	return dependencyChanged(deps.Config.RepoPath, inputs, changedFiles), true
}

func touchedStackDirs(repoPath string, stacks []compose.Stack, changedFiles []string) map[string]struct{} {
	dirs := make([]string, 0, len(stacks))
	for _, s := range stacks {
		if s.Dir != "" {
			dir := resolveUnderRepo(repoPath, s.Dir)
			if !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	touched := make(map[string]struct{}, len(dirs))
	for _, f := range changedFiles {
		abs := resolveUnderRepo(repoPath, f)
		best := ""
		for _, dir := range dirs {
			if pathUnder(dir, abs) && len(dir) > len(best) {
				best = dir
			}
		}
		if best != "" {
			touched[best] = struct{}{}
		}
	}
	return touched
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
	base := context.WithoutCancel(ctx)

	failures := make([]error, len(stacks))
	sem := make(chan struct{}, maxConcurrentTeardowns)
	var wg sync.WaitGroup
	for i, s := range stacks {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			deps.Log.Info("tearing down compose stack", "project", s.ProjectName, "reason", reason)
			failures[i] = downStack(base, deps, s.ProjectName)
		})
	}
	wg.Wait()

	var errs []error
	var removed, remaining []string
	for i, s := range stacks {
		if err := failures[i]; err != nil {
			deps.Log.Error("tearing down compose stack failed", "project", s.ProjectName, "err", err)
			errs = append(errs, fmt.Errorf("tearing down %s: %w", s.ProjectName, err))
			remaining = append(remaining, s.ProjectName)
			continue
		}
		removed = append(removed, s.ProjectName)
	}
	if len(errs) == 0 {
		return nil
	}
	errs = append(errs, fmt.Errorf("torn down: %s; still running: %s", listOrNone(removed), listOrNone(remaining)))
	return errors.Join(errs...)
}

func downStack(ctx context.Context, deps Deps, projectName string) error {
	ctx, cancel := context.WithTimeout(ctx, teardownTimeout)
	defer cancel()
	return deps.Compose.Down(ctx, projectName)
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
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

func updatedServices(cfg *config.Config, before, after map[string]health.Snapshot, stacks []compose.Stack) (updated, restarted []string, known bool) {
	if cfg.HealthWatchSeconds <= 0 {
		return nil, nil, false
	}
	for _, s := range stacks {
		post, ok := after[s.ProjectName]
		if !ok {
			return nil, nil, false
		}
		updated = append(updated, health.ChangedServices(before[s.ProjectName], post)...)
		restarted = append(restarted, health.RestartedServices(before[s.ProjectName], post)...)
	}
	slices.Sort(updated)
	slices.Sort(restarted)
	return slices.Compact(updated), slices.Compact(restarted), true
}

func recordOutcome(deps Deps, st *state.State, result state.Result, commit string) {
	next := *st
	next.LastResult = result
	next.LastAttemptAt = deps.Clock.Now()
	if commit != "" {
		next.LastAttemptCommit = commit
	}
	if err := deps.State.Save(&next); err != nil {
		deps.Log.Error("saving state", "err", err, "last_result", string(result))
		return
	}
	*st = next
}

func promoteHealthy(deps Deps, st *state.State, commit string, stacks []string) error {
	now := deps.Clock.Now()
	next := *st
	next.LastHealthyCommit = commit
	next.LastHealthyAt = now
	next.LastHealthyStacks = slices.Clone(stacks)
	next.LastCheckoutCommit = commit
	next.PendingCommit = ""
	next.PendingSince = time.Time{}
	next.PendingAttempts = 0
	next.PendingStacks = nil
	next.PendingRevert = false
	next.PreflightBlocks = 0
	next.ForgetAllFailed()
	next.LastAttemptCommit = commit
	next.LastAttemptAt = now
	next.LastResult = state.ResultSuccess
	if err := deps.State.Save(&next); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	*st = next
	return nil
}
