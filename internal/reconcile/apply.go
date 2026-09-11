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

const restoreCheckoutTimeout = 30 * time.Second

func runNormal(ctx context.Context, opts Options, deps Deps, st *state.State, start time.Time) Result {
	cfg := deps.Config
	retryDelay := time.Duration(cfg.RetryDelaySeconds) * time.Second

	gitResult, err := git.Fetch(ctx, deps.Git, cfg.RetryAttempts, retryDelay, deps.Log)
	if err != nil {
		deps.Log.Error("git sync failed", "err", err)
		if ctx.Err() != nil {
			return Result{Err: err}
		}
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
		if ctx.Err() != nil {
			return result
		}
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
		revertResult := doRevert(ctx, deps, st, gitResult.NewCommit, "pre-flight: live stack unhealthy: "+reason, start)
		revertResult.Changed = result.Changed
		revertResult.OldCommit = result.OldCommit
		revertResult.NewCommit = result.NewCommit
		revertResult.ChangedFiles = result.ChangedFiles
		return revertResult
	}

	return applyAndWatch(ctx, deps, st, result, start)
}

func applyAndWatch(ctx context.Context, deps Deps, st *state.State, result Result, start time.Time) Result {
	cfg := deps.Config
	commit := result.NewCommit

	if err := deps.Git.Checkout(ctx, commit); err != nil {
		return abortBeforeUp(ctx, deps, result, "checkout failed", err)
	}

	project, err := deps.Compose.LoadProject(ctx, cfg.ComposeFile, cfg.ProjectName)
	if err != nil {
		return abortBeforeUp(ctx, deps, result, "loading compose project failed", fmt.Errorf("loading project: %w", err))
	}

	if err := compose.CheckEnvFiles(project); err != nil {
		return abortBeforeUp(ctx, deps, result, "env_file missing", err)
	}

	// Persisted before touching Docker so a crash here is recoverable via
	// pending_commit on the next run.
	now := deps.Clock.Now()
	st.PendingCommit = commit
	st.PendingSince = now
	st.LastAttemptCommit = commit
	st.LastAttemptAt = now
	if st.LastFailedCommit == commit {
		st.LastFailedCommit = ""
		st.LastFailedAt = time.Time{}
	}
	if err := deps.State.Save(st); err != nil {
		return abortBeforeUp(ctx, deps, result, "saving state failed", fmt.Errorf("saving state: %w", err))
	}

	if err := deps.Compose.Up(ctx, project); err != nil {
		if ctx.Err() != nil {
			result.Err = fmt.Errorf("compose up interrupted: %w", err)
			return result
		}
		deps.Log.Error("compose up failed, reverting", "err", err)
		return failAndRevert(ctx, deps, st, result, fmt.Sprintf("applying %s failed: %v", shortCommit(commit), err), start)
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
		st.LastHealthyCommit = commit
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
			Commit:      commit,
			Branch:      cfg.Branch,
			Services:    result.Services,
			HealthWatch: watchOpts.WatchDuration,
			Duration:    time.Since(start),
		})
		result.Notification = &embed
		return result
	}

	deps.Log.Warn("health watch failed, reverting", "reason", watchResult.Reason)
	return failAndRevert(ctx, deps, st, result, fmt.Sprintf("applied %s failed health watch (%s)", shortCommit(commit), watchResult.Reason), start)
}

func failAndRevert(ctx context.Context, deps Deps, st *state.State, result Result, reason string, start time.Time) Result {
	st.LastFailedCommit = result.NewCommit
	st.LastFailedAt = deps.Clock.Now()
	st.LastResult = state.ResultFailedApply
	if err := deps.State.Save(st); err != nil {
		deps.Log.Error("saving state before revert", "err", err)
	}

	revertResult := doRevert(ctx, deps, st, result.NewCommit, reason, start)
	result.Reverted = revertResult.Reverted
	result.Degraded = revertResult.Degraded
	result.RolledBackTo = revertResult.RolledBackTo
	result.Err = revertResult.Err
	result.Notification = revertResult.Notification
	result.HealthWatch += revertResult.HealthWatch
	return result
}

func abortBeforeUp(ctx context.Context, deps Deps, result Result, title string, err error) Result {
	deps.Log.Error(title, "err", err)
	restoreCheckout(ctx, deps, result.OldCommit)
	result.Err = err
	if ctx.Err() != nil {
		return result
	}
	embed := notify.BuildEmbed(notify.Report{
		Outcome: notify.OutcomeFailure,
		Title:   "ComposeLock: " + title,
		Commit:  result.NewCommit,
		Branch:  deps.Config.Branch,
		Err:     err,
	})
	result.Notification = &embed
	return result
}

func restoreCheckout(ctx context.Context, deps Deps, commit string) {
	if commit == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreCheckoutTimeout)
	defer cancel()
	if err := deps.Git.Checkout(ctx, commit); err != nil {
		deps.Log.Error("restoring previous checkout failed", "commit", commit, "err", err)
	}
}
