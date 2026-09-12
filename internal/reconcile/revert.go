package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

func doRevert(ctx context.Context, deps Deps, st *state.State, failedCommit, reason string, start time.Time) Result {
	cfg := deps.Config
	target := st.LastHealthyCommit

	if target == "" {
		err := fmt.Errorf("no rollback target available (%s)", reason)
		deps.Log.Error("revert: no last_healthy_commit, DEGRADED", "reason", reason)
		return degrade(deps, st, failedCommit, "", err, start)
	}

	if err := deps.Git.Checkout(ctx, target); err != nil {
		return revertFailed(ctx, deps, st, failedCommit, target, fmt.Errorf("checking out rollback target %s: %w", target, err), start)
	}

	project, err := deps.Compose.LoadProject(ctx, cfg.ComposeFile, cfg.ProjectName)
	if err != nil {
		return revertFailed(ctx, deps, st, failedCommit, target, fmt.Errorf("loading project for revert: %w", err), start)
	}

	if err := deps.Compose.Up(ctx, project); err != nil {
		return revertFailed(ctx, deps, st, failedCommit, target, fmt.Errorf("compose up during revert: %w", err), start)
	}

	watchOpts := healthOptions(cfg, target)
	watchResult, err := health.Watch(ctx, deps.Health, deps.Clock, watchOpts, deps.Log)
	if err != nil {
		// ctx cancelled: state is left as-is (pending_commit still set),
		// so the next run picks this back up via crash recovery.
		return Result{RolledBackTo: target, Err: fmt.Errorf("health watch during revert: %w", err)}
	}

	if watchResult.Outcome != health.Healthy {
		return degrade(deps, st, failedCommit, target,
			fmt.Errorf("revert to %s also failed health watch (%s): manual intervention required", shortCommit(target), watchResult.Reason),
			start)
	}

	st.PendingCommit = ""
	st.PendingSince = time.Time{}
	st.LastResult = state.ResultReverted
	if err := deps.State.Save(st); err != nil {
		return Result{Reverted: true, RolledBackTo: target, HealthWatch: watchOpts.WatchDuration, Err: fmt.Errorf("saving state: %w", err)}
	}

	revertErr := fmt.Errorf("%s, reverted to %s — healthy again", reason, shortCommit(target))
	embed := notify.BuildEmbed(notify.Report{
		Outcome:     notify.OutcomeRecovered,
		Title:       "ComposeLock: reverted, healthy again",
		Commit:      failedCommit,
		Branch:      cfg.Branch,
		HealthWatch: watchOpts.WatchDuration,
		Duration:    time.Since(start),
		Err:         revertErr,
	})
	return Result{Reverted: true, RolledBackTo: target, HealthWatch: watchOpts.WatchDuration, Err: revertErr, Notification: &embed}
}

func revertFailed(ctx context.Context, deps Deps, st *state.State, failedCommit, target string, err error, start time.Time) Result {
	if ctx.Err() != nil {
		deps.Log.Warn("revert interrupted", "err", err)
		return Result{RolledBackTo: target, Err: fmt.Errorf("revert interrupted: %w", err)}
	}
	return degrade(deps, st, failedCommit, target, err, start)
}

func degrade(deps Deps, st *state.State, failedCommit, target string, err error, start time.Time) Result {
	cfg := deps.Config
	st.LastResult = state.ResultDegraded
	if saveErr := deps.State.Save(st); saveErr != nil {
		deps.Log.Error("saving degraded state", "err", saveErr)
	}
	embed := notify.BuildEmbed(notify.Report{
		Outcome:  notify.OutcomeFailure,
		Title:    "ComposeLock: DEGRADED — manual intervention required",
		Commit:   failedCommit,
		Branch:   cfg.Branch,
		Duration: time.Since(start),
		Err:      err,
	})
	return Result{Degraded: true, RolledBackTo: target, Err: err, Notification: &embed}
}
