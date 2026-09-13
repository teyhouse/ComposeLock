package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/teyhouse/ComposeLock/internal/state"
)

const maxRecoveryAttempts = 3

const recoveryAttemptGrace = 15 * time.Minute

func recoverFromCrash(ctx context.Context, opts Options, deps Deps, st *state.State, start time.Time) Result {
	commit := st.PendingCommit

	if opts.DryRun {
		deps.Log.Info("dry run: pending commit detected from a previous interrupted run", "pending_commit", commit)
		return Result{NewCommit: commit}
	}

	if opts.Force {
		st.PendingAttempts = 0
		st.PendingSince = deps.Clock.Now()
	}
	if st.PendingAttempts >= maxRecoveryAttempts {
		deps.Log.Error("crash recovery: attempt limit reached, DEGRADED",
			"pending_commit", commit, "attempts", st.PendingAttempts)
		return degrade(deps, st, commit, "", nil,
			fmt.Errorf("crash recovery for %s failed %d times: manual intervention required", shortCommit(commit), st.PendingAttempts),
			start)
	}
	if budget, expired := recoveryExpired(deps, st); expired {
		deps.Log.Error("crash recovery: pending for longer than the recovery budget, DEGRADED",
			"pending_commit", commit, "pending_since", st.PendingSince, "budget", budget.String())
		return degrade(deps, st, commit, "", nil,
			fmt.Errorf("crash recovery for %s has been pending longer than %s: manual intervention required", shortCommit(commit), budget),
			start)
	}

	discovered, err := previousStacksAt(deps)
	if err != nil {
		return Result{NewCommit: commit, Err: fmt.Errorf("discovering compose stacks: %w", err)}
	}

	if st.RevertInProgress() {
		if err := countRecoveryAttempt(deps, st); err != nil {
			return Result{NewCommit: commit, Err: err}
		}
		deps.Log.Warn("crash recovery: resuming interrupted revert", "pending_commit", commit, "attempt", st.PendingAttempts)
		stacks := restrictToNames(discovered, st.PendingStacks)
		result := doRevert(ctx, deps, st, stacks, commit, fmt.Sprintf("crash recovery: resumed revert of %s", shortCommit(commit)), start)
		result.NewCommit = commit
		return result
	}

	live, err := evaluateLive(ctx, deps, discovered)
	if err != nil {
		return Result{NewCommit: commit, Err: fmt.Errorf("crash recovery: live snapshot: %w", err)}
	}

	if err := countRecoveryAttempt(deps, st); err != nil {
		return Result{NewCommit: commit, Err: err}
	}

	if !live.healthy {
		deps.Log.Warn("crash recovery: live stack unhealthy, reverting immediately", "reason", live.reason)
		stacks := restrictToNames(discovered, st.PendingStacks)
		result := doRevert(ctx, deps, st, stacks, commit, "crash recovery: live stack unhealthy: "+live.reason, start)
		result.NewCommit = commit
		return result
	}

	deps.Log.Info("crash recovery: live stack healthy, re-applying pending commit",
		"pending_commit", commit, "attempt", st.PendingAttempts)
	return applyAndWatch(ctx, deps, st,
		Result{NewCommit: commit, OldCommit: st.LastHealthyCommit},
		deployedStacks(st, discovered), st.PendingStacks, live.snapshots, start)
}

func countRecoveryAttempt(deps Deps, st *state.State) error {
	next := *st
	next.PendingAttempts++
	if err := deps.State.Save(&next); err != nil {
		deps.Log.Error("crash recovery: saving attempt counter", "err", err)
		return fmt.Errorf("saving state: %w", err)
	}
	*st = next
	return nil
}

func recoveryExpired(deps Deps, st *state.State) (time.Duration, bool) {
	budget := maxRecoveryAttempts * (time.Duration(deps.Config.HealthWatchSeconds)*time.Second + recoveryAttemptGrace)
	if st.PendingSince.IsZero() {
		return budget, false
	}
	return budget, deps.Clock.Now().Sub(st.PendingSince) > budget
}
