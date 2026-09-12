package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/teyhouse/ComposeLock/internal/state"
)

func recoverFromCrash(ctx context.Context, opts Options, deps Deps, st *state.State, start time.Time) Result {
	commit := st.PendingCommit

	if opts.DryRun {
		deps.Log.Info("dry run: pending commit detected from a previous interrupted run", "pending_commit", commit)
		return Result{NewCommit: commit}
	}

	stacks, stacksErr := StacksFor(deps.Config)

	if st.RevertInProgress() {
		deps.Log.Warn("crash recovery: resuming interrupted revert", "pending_commit", commit)
		if stacksErr != nil {
			return Result{NewCommit: commit, Err: fmt.Errorf("discovering compose stacks: %w", stacksErr)}
		}
		result := doRevert(ctx, deps, st, stacks, commit, fmt.Sprintf("crash recovery: resumed revert of %s", shortCommit(commit)), start)
		result.NewCommit = commit
		return result
	}

	liveHealthy, reason, err := evaluateLive(ctx, deps, stacks)
	if err != nil {
		return Result{Err: fmt.Errorf("crash recovery: live snapshot: %w", err)}
	}
	if !liveHealthy {
		deps.Log.Warn("crash recovery: live stack unhealthy, reverting immediately", "reason", reason)
		if stacksErr != nil {
			return Result{Err: fmt.Errorf("discovering compose stacks: %w", stacksErr)}
		}
		result := doRevert(ctx, deps, st, stacks, commit, "crash recovery: live stack unhealthy: "+reason, start)
		result.NewCommit = commit
		return result
	}

	deps.Log.Info("crash recovery: live stack healthy, re-applying pending commit", "pending_commit", commit)
	return applyAndWatch(ctx, deps, st, Result{NewCommit: commit}, stacks, start)
}
