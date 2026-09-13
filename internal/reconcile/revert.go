package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

func doRevert(ctx context.Context, deps Deps, st *state.State, stacks []compose.Stack, failedCommit, reason string, start time.Time) Result {
	cfg := deps.Config
	target := st.LastHealthyCommit

	if target == "" {
		err := fmt.Errorf("no rollback target available (%s)", reason)
		deps.Log.Error("revert: no last_healthy_commit, DEGRADED", "reason", reason)
		return degrade(deps, st, failedCommit, "", stackNames(stacks), err, start)
	}

	if err := deps.Git.Checkout(ctx, target); err != nil {
		return revertFailed(ctx, deps, st, failedCommit, target, stacks, fmt.Errorf("checking out rollback target %s: %w", target, err), start)
	}

	targetStacks, err := StacksFor(cfg)
	if err != nil {
		return revertFailed(ctx, deps, st, failedCommit, target, nil, fmt.Errorf("discovering compose stacks at rollback target: %w", err), start)
	}
	revertStacks := intersectByProjectName(targetStacks, stacks)

	tearDownStacks(ctx, deps, vanishedStacks(stacks, targetStacks), "stack does not exist at rollback target")

	var allServices []string
	for _, s := range revertStacks {
		project, err := deps.Compose.LoadProject(ctx, s.Files, s.ProjectName)
		if err != nil {
			return revertFailed(ctx, deps, st, failedCommit, target, revertStacks, fmt.Errorf("loading project %s for revert: %w", s.ProjectName, err), start)
		}
		if err := deps.Compose.Up(ctx, project); err != nil {
			return revertFailed(ctx, deps, st, failedCommit, target, revertStacks, fmt.Errorf("compose up during revert of %s: %w", s.ProjectName, err), start)
		}
		allServices = append(allServices, serviceNames(project)...)
	}

	watchResult, err := watchStacks(ctx, deps, revertStacks, target)
	if err != nil {
		// ctx cancelled: state is left as-is (pending_commit still set),
		// so the next run picks this back up via crash recovery.
		return Result{RolledBackTo: target, Err: fmt.Errorf("health watch during revert: %w", err)}
	}
	watchDuration := time.Duration(cfg.HealthWatchSeconds) * time.Second

	if watchResult.Outcome != health.Healthy {
		return degrade(deps, st, failedCommit, target, stackNames(revertStacks),
			fmt.Errorf("revert to %s also failed health watch (%s): manual intervention required", shortCommit(target), watchResult.Reason),
			start)
	}

	st.PendingCommit = ""
	st.PendingSince = time.Time{}
	st.PendingAttempts = 0
	st.LastResult = state.ResultReverted
	if err := deps.State.Save(st); err != nil {
		return Result{Reverted: true, RolledBackTo: target, HealthWatch: watchDuration, Err: fmt.Errorf("saving state: %w", err)}
	}

	revertErr := fmt.Errorf("%s, reverted to %s — healthy again", reason, shortCommit(target))
	embed := notify.BuildEmbed(notify.Report{
		Outcome:     notify.OutcomeRecovered,
		Title:       "ComposeLock: reverted, healthy again",
		Commit:      failedCommit,
		Branch:      cfg.Branch,
		Services:    allServices,
		Stacks:      reportStacks(cfg, stackNames(revertStacks)),
		HealthWatch: watchDuration,
		Duration:    time.Since(start),
		Err:         revertErr,
	})
	return Result{Reverted: true, RolledBackTo: target, HealthWatch: watchDuration, Err: revertErr, Notification: &embed, NotificationKey: "reverted:" + failedCommit}
}

func revertFailed(ctx context.Context, deps Deps, st *state.State, failedCommit, target string, stacks []compose.Stack, err error, start time.Time) Result {
	if ctx.Err() != nil {
		deps.Log.Warn("revert interrupted", "err", err)
		return Result{RolledBackTo: target, Err: fmt.Errorf("revert interrupted: %w", err)}
	}
	return degrade(deps, st, failedCommit, target, stackNames(stacks), err, start)
}

func degrade(deps Deps, st *state.State, failedCommit, target string, stacks []string, err error, start time.Time) Result {
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
		Stacks:   reportStacks(cfg, stacks),
		Duration: time.Since(start),
		Err:      err,
	})
	return Result{Degraded: true, RolledBackTo: target, Err: err, Notification: &embed, NotificationKey: "degraded:" + failedCommit}
}
