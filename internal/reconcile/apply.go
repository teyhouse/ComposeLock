package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

func runNormal(ctx context.Context, opts Options, deps Deps, st *state.State, start time.Time) Result {
	cfg := deps.Config
	retryDelay := time.Duration(cfg.RetryDelaySeconds) * time.Second

	var gitResult git.Result
	var err error
	if opts.DryRun {
		gitResult, err = git.SyncPreview(ctx, deps.Git, cfg.RetryAttempts, retryDelay, deps.Log)
	} else {
		gitResult, err = git.Sync(ctx, deps.Git, cfg.RetryAttempts, retryDelay, deps.Log)
	}
	if err != nil {
		deps.Log.Error("git sync failed", "err", err)
		embed := notify.BuildEmbed(notify.Report{
			Outcome: notify.OutcomeFailure,
			Title:   "ComposeLock: git sync failed",
			Branch:  cfg.Branch,
			Err:     err,
		})
		return Result{Err: err, Notification: &embed}
	}

	result := Result{
		Changed:      gitResult.Changed,
		OldCommit:    gitResult.OldCommit,
		NewCommit:    gitResult.NewCommit,
		ChangedFiles: gitResult.ChangedFiles,
	}
	if !gitResult.Changed {
		deps.Log.Info("nothing to do", "commit", gitResult.OldCommit)
		return result
	}

	// A revert moves the local checkout back to last_healthy_commit but
	// never moves origin/<branch>, so without this guard the next
	// Reconcile would see local != remote again and re-apply the same bad
	// commit forever.
	if gitResult.NewCommit == st.LastFailedCommit && !opts.Force {
		deps.Log.Info("skipping known-bad commit", "commit", gitResult.NewCommit)
		result.Skipped = true
		return result
	}

	snap, err := deps.Health.Snapshot(ctx, cfg.ProjectName)
	if err != nil {
		deps.Log.Error("pre-flight snapshot failed", "err", err)
		result.Err = fmt.Errorf("pre-flight snapshot: %w", err)
		embed := notify.BuildEmbed(notify.Report{
			Outcome: notify.OutcomeFailure,
			Title:   "ComposeLock: pre-flight check failed",
			Commit:  gitResult.NewCommit,
			Branch:  cfg.Branch,
			Err:     result.Err,
		})
		result.Notification = &embed
		return result
	}
	liveHealthy, reason := health.Evaluate(snap)

	if opts.DryRun {
		deps.Log.Info("dry run: would apply", "old_commit", gitResult.OldCommit, "new_commit", gitResult.NewCommit, "live_healthy", liveHealthy, "gate_reason", reason)
		return result
	}

	if !liveHealthy {
		deps.Log.Warn("pre-flight gate: live stack unhealthy, not applying", "reason", reason)
		st.LastResult = state.ResultFailedPreflight
		revertResult := doRevert(ctx, deps, st, gitResult.NewCommit, "pre-flight: live stack unhealthy: "+reason, start)
		revertResult.Changed = result.Changed
		revertResult.OldCommit = result.OldCommit
		revertResult.NewCommit = result.NewCommit
		revertResult.ChangedFiles = result.ChangedFiles
		return revertResult
	}

	return applyAndWatch(ctx, deps, st, gitResult, result, start)
}

func applyAndWatch(ctx context.Context, deps Deps, st *state.State, gitResult git.Result, result Result, start time.Time) Result {
	cfg := deps.Config

	project, err := deps.Compose.LoadProject(ctx, cfg.ComposeFile, cfg.ProjectName)
	if err != nil {
		result.Err = fmt.Errorf("loading project: %w", err)
		deps.Log.Error("loading compose project failed", "err", err)
		embed := notify.BuildEmbed(notify.Report{
			Outcome: notify.OutcomeFailure,
			Title:   "ComposeLock: loading compose project failed",
			Commit:  gitResult.NewCommit,
			Branch:  cfg.Branch,
			Err:     result.Err,
		})
		result.Notification = &embed
		return result
	}

	if err := compose.CheckEnvFiles(project); err != nil {
		result.Err = err
		deps.Log.Error("env_file check failed", "err", err)
		embed := notify.BuildEmbed(notify.Report{
			Outcome: notify.OutcomeFailure,
			Title:   "ComposeLock: env_file missing",
			Commit:  gitResult.NewCommit,
			Branch:  cfg.Branch,
			Err:     result.Err,
		})
		result.Notification = &embed
		return result
	}

	// Persisted before touching Docker so a crash here is recoverable via
	// pending_commit on the next run.
	now := deps.Clock.Now()
	st.PendingCommit = gitResult.NewCommit
	st.PendingSince = now
	st.LastAttemptCommit = gitResult.NewCommit
	st.LastAttemptAt = now
	if err := deps.State.Save(st); err != nil {
		result.Err = fmt.Errorf("saving state: %w", err)
		return result
	}

	if err := deps.Compose.Up(ctx, project); err != nil {
		deps.Log.Error("compose up failed", "err", err)
		st.LastFailedCommit = gitResult.NewCommit
		st.LastFailedAt = deps.Clock.Now()
		st.LastResult = state.ResultFailedApply
		if saveErr := deps.State.Save(st); saveErr != nil {
			deps.Log.Error("saving state after failed apply", "err", saveErr)
		}
		result.Err = fmt.Errorf("compose up: %w", err)
		embed := notify.BuildEmbed(notify.Report{
			Outcome: notify.OutcomeFailure,
			Title:   "ComposeLock: apply failed",
			Commit:  gitResult.NewCommit,
			Branch:  cfg.Branch,
			Err:     result.Err,
		})
		result.Notification = &embed
		return result
	}

	result.Applied = true
	result.Services = serviceNames(project)

	watchOpts := healthOptions(cfg)
	watchResult, err := health.Watch(ctx, deps.Health, deps.Clock, watchOpts, deps.Log)
	if err != nil {
		result.Err = fmt.Errorf("health watch: %w", err)
		return result
	}
	result.HealthWatch = watchOpts.WatchDuration

	if watchResult.Outcome == health.Healthy {
		st.LastHealthyCommit = gitResult.NewCommit
		st.LastHealthyAt = deps.Clock.Now()
		st.PendingCommit = ""
		st.PendingSince = time.Time{}
		st.LastFailedCommit = ""
		st.LastFailedAt = time.Time{}
		st.LastResult = state.ResultSuccess
		if err := deps.State.Save(st); err != nil {
			result.Err = fmt.Errorf("saving state: %w", err)
			return result
		}
		embed := notify.BuildEmbed(notify.Report{
			Outcome:     notify.OutcomeSuccess,
			Title:       "ComposeLock: deployed successfully",
			Commit:      gitResult.NewCommit,
			Branch:      cfg.Branch,
			Services:    result.Services,
			HealthWatch: watchOpts.WatchDuration,
			Duration:    time.Since(start),
		})
		result.Notification = &embed
		return result
	}

	deps.Log.Warn("health watch failed, reverting", "reason", watchResult.Reason)
	st.LastFailedCommit = gitResult.NewCommit
	st.LastFailedAt = deps.Clock.Now()
	st.LastResult = state.ResultFailedApply

	revertResult := doRevert(ctx, deps, st, gitResult.NewCommit, watchResult.Reason, start)
	result.Reverted = revertResult.Reverted
	result.Degraded = revertResult.Degraded
	result.RolledBackTo = revertResult.RolledBackTo
	result.Err = revertResult.Err
	result.Notification = revertResult.Notification
	result.HealthWatch += revertResult.HealthWatch
	return result
}
