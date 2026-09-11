package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/state"
)

func recoverFromCrash(ctx context.Context, opts Options, deps Deps, st *state.State, start time.Time) Result {
	commit := st.PendingCommit

	if opts.DryRun {
		deps.Log.Info("dry run: pending commit detected from a previous interrupted run", "pending_commit", commit)
		return Result{NewCommit: commit}
	}

	if st.RevertInProgress() {
		deps.Log.Warn("crash recovery: resuming interrupted revert", "pending_commit", commit)
		result := doRevert(ctx, deps, st, commit, fmt.Sprintf("crash recovery: resumed revert of %s", shortCommit(commit)), start)
		result.NewCommit = commit
		return result
	}

	snap, err := deps.Health.Snapshot(ctx, deps.Config.ProjectName)
	if err != nil {
		return Result{Err: fmt.Errorf("crash recovery: live snapshot: %w", err)}
	}
	if liveHealthy, reason := health.Evaluate(snap); !liveHealthy {
		deps.Log.Warn("crash recovery: live stack unhealthy, reverting immediately", "reason", reason)
		result := doRevert(ctx, deps, st, commit, "crash recovery: live stack unhealthy: "+reason, start)
		result.NewCommit = commit
		return result
	}

	deps.Log.Info("crash recovery: live stack healthy, re-applying pending commit", "pending_commit", commit)
	return applyAndWatch(ctx, deps, st, Result{NewCommit: commit}, start)
}
