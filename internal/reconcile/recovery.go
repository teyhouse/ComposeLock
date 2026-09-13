package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/teyhouse/ComposeLock/internal/state"
)

const maxRecoveryAttempts = 3

func recoverFromCrash(ctx context.Context, opts Options, deps Deps, st *state.State, start time.Time) Result {
	commit := st.PendingCommit

	if opts.DryRun {
		deps.Log.Info("dry run: pending commit detected from a previous interrupted run", "pending_commit", commit)
		return Result{NewCommit: commit}
	}

	if opts.Force {
		st.PendingAttempts = 0
	}
	if st.PendingAttempts >= maxRecoveryAttempts {
		deps.Log.Error("crash recovery: attempt limit reached, DEGRADED",
			"pending_commit", commit, "attempts", st.PendingAttempts)
		return degrade(deps, st, commit, "", nil,
			fmt.Errorf("crash recovery for %s failed %d times: manual intervention required", shortCommit(commit), st.PendingAttempts),
			start)
	}

	st.PendingAttempts++
	if err := deps.State.Save(st); err != nil {
		deps.Log.Error("crash recovery: saving attempt counter", "err", err)
		return Result{NewCommit: commit, Err: fmt.Errorf("saving state: %w", err)}
	}

	stacks, stacksErr := StacksFor(deps.Config)

	if st.RevertInProgress() {
		deps.Log.Warn("crash recovery: resuming interrupted revert", "pending_commit", commit, "attempt", st.PendingAttempts)
		if stacksErr != nil {
			return Result{NewCommit: commit, Err: fmt.Errorf("discovering compose stacks: %w", stacksErr)}
		}
		result := doRevert(ctx, deps, st, stacks, commit, fmt.Sprintf("crash recovery: resumed revert of %s", shortCommit(commit)), start)
		result.NewCommit = commit
		return result
	}

	liveHealthy, reason, err := evaluateLive(ctx, deps, stacks)
	if err != nil {
		return Result{NewCommit: commit, Err: fmt.Errorf("crash recovery: live snapshot: %w", err)}
	}
	if !liveHealthy {
		deps.Log.Warn("crash recovery: live stack unhealthy, reverting immediately", "reason", reason)
		if stacksErr != nil {
			return Result{NewCommit: commit, Err: fmt.Errorf("discovering compose stacks: %w", stacksErr)}
		}
		result := doRevert(ctx, deps, st, stacks, commit, "crash recovery: live stack unhealthy: "+reason, start)
		result.NewCommit = commit
		return result
	}

	deps.Log.Info("crash recovery: live stack healthy, re-applying pending commit",
		"pending_commit", commit, "attempt", st.PendingAttempts)
	return applyAndWatch(ctx, deps, st, Result{NewCommit: commit, OldCommit: st.LastHealthyCommit}, stacks, start)
}
