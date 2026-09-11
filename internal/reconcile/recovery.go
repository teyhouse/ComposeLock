package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

func recoverFromCrash(ctx context.Context, opts Options, deps Deps, st *state.State, start time.Time) Result {
	cfg := deps.Config

	if st.LastResult == state.ResultDegraded && !opts.Force {
		deps.Log.Warn("refusing crash recovery: system is DEGRADED, use --force", "pending_commit", st.PendingCommit)
		return Result{Degraded: true, NewCommit: st.PendingCommit, Err: errDegraded}
	}

	if opts.DryRun {
		deps.Log.Info("dry run: pending commit detected from a previous interrupted run", "pending_commit", st.PendingCommit)
		return Result{NewCommit: st.PendingCommit}
	}

	snap, err := deps.Health.Snapshot(ctx, cfg.ProjectName)
	if err != nil {
		return Result{Err: fmt.Errorf("crash recovery: live snapshot: %w", err)}
	}
	liveHealthy, reason := health.Evaluate(snap)

	if !liveHealthy {
		deps.Log.Warn("crash recovery: live stack unhealthy, reverting immediately", "reason", reason)
		result := doRevert(ctx, deps, st, st.PendingCommit, "crash recovery: live stack unhealthy: "+reason, start)
		result.NewCommit = st.PendingCommit
		return result
	}

	deps.Log.Info("crash recovery: live stack healthy, running fresh health watch", "pending_commit", st.PendingCommit)
	watchOpts := healthOptions(cfg)
	watchResult, err := health.Watch(ctx, deps.Health, deps.Clock, watchOpts, deps.Log)
	if err != nil {
		return Result{Err: fmt.Errorf("crash recovery: health watch: %w", err)}
	}

	commit := st.PendingCommit

	if watchResult.Outcome == health.Healthy {
		st.LastHealthyCommit = commit
		st.LastHealthyAt = deps.Clock.Now()
		st.PendingCommit = ""
		st.PendingSince = time.Time{}
		st.LastFailedCommit = ""
		st.LastFailedAt = time.Time{}
		st.LastResult = state.ResultSuccess
		if err := deps.State.Save(st); err != nil {
			return Result{Applied: true, NewCommit: commit, HealthWatch: watchOpts.WatchDuration, Err: fmt.Errorf("saving state: %w", err)}
		}
		embed := notify.BuildEmbed(notify.Report{
			Outcome:     notify.OutcomeSuccess,
			Title:       "ComposeLock: crash recovery — deployed successfully",
			Commit:      commit,
			Branch:      cfg.Branch,
			HealthWatch: watchOpts.WatchDuration,
			Duration:    time.Since(start),
		})
		return Result{Applied: true, NewCommit: commit, HealthWatch: watchOpts.WatchDuration, Notification: &embed}
	}

	deps.Log.Warn("crash recovery: fresh health watch failed, reverting", "reason", watchResult.Reason)
	st.LastFailedCommit = commit
	st.LastFailedAt = deps.Clock.Now()
	st.LastResult = state.ResultFailedApply

	result := doRevert(ctx, deps, st, commit, watchResult.Reason, start)
	result.Applied = true
	result.NewCommit = commit
	return result
}
