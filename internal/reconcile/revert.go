package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

const preflightRecovery = ""

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
	st.LastCheckoutCommit = target

	targetStacks, err := stacksAllowingEmpty(deps)
	if err != nil {
		return revertFailed(ctx, deps, st, failedCommit, target, stacks, fmt.Errorf("discovering compose stacks at rollback target: %w", err), start)
	}
	revertStacks := intersectByProjectName(targetStacks, stacks)

	if err := tearDownStacks(ctx, deps, vanishedStacks(stacks, targetStacks), "stack does not exist at rollback target"); err != nil {
		return revertFailed(ctx, deps, st, failedCommit, target, revertStacks, err, start)
	}

	var allServices []string
	projects := make([]*types.Project, len(revertStacks))
	for i, s := range revertStacks {
		project, err := deps.Compose.LoadProject(ctx, s.Files, s.ProjectName)
		if err != nil {
			return revertFailed(ctx, deps, st, failedCommit, target, revertStacks, fmt.Errorf("loading project %s for revert: %w", s.ProjectName, err), start)
		}
		if err := compose.CheckEnvFiles(project); err != nil {
			return revertFailed(ctx, deps, st, failedCommit, target, revertStacks, fmt.Errorf("env_file missing for revert of %s: %w", s.ProjectName, err), start)
		}
		projects[i] = project
	}
	for i, s := range revertStacks {
		if err := deps.Compose.Up(ctx, projects[i]); err != nil {
			return revertFailed(ctx, deps, st, failedCommit, target, revertStacks, fmt.Errorf("compose up during revert of %s: %w", s.ProjectName, err), start)
		}
		allServices = append(allServices, serviceNames(projects[i])...)
	}

	watchResult, _, err := watchStacks(ctx, deps, revertStacks, projects, target)
	if err != nil {
		return revertInterrupted(ctx, deps, st, failedCommit, target, revertStacks, fmt.Errorf("health watch during revert: %w", err), start)
	}
	watchDuration := time.Duration(cfg.HealthWatchSeconds) * time.Second

	if watchResult.Outcome != health.Healthy {
		return degrade(deps, st, failedCommit, target, stackNames(revertStacks),
			fmt.Errorf("revert to %s also failed health watch (%s): manual intervention required", shortCommit(target), watchResult.Reason),
			start)
	}

	next := *st
	next.PendingCommit = ""
	next.PendingSince = time.Time{}
	next.PendingAttempts = 0
	next.PendingStacks = nil
	next.PendingRevert = false
	next.LastCheckoutCommit = target
	next.LastAttemptAt = deps.Clock.Now()
	if failedCommit != preflightRecovery {
		next.LastAttemptCommit = failedCommit
	}
	next.LastResult = state.ResultReverted
	if err := deps.State.Save(&next); err != nil {
		return Result{Reverted: true, RolledBackTo: target, HealthWatch: watchDuration, Err: fmt.Errorf("saving state: %w", err)}
	}
	*st = next

	title, key := "ComposeLock: reverted, healthy again", "reverted:"+failedCommit
	if failedCommit == preflightRecovery {
		title, key = "ComposeLock: restored last healthy commit", "preflight-recovered:"+target
	}
	revertErr := fmt.Errorf("%s, reverted to %s: healthy again", reason, shortCommit(target))
	embed := notify.BuildEmbed(notify.Report{
		Outcome:     notify.OutcomeRecovered,
		Title:       title,
		Commit:      reportedCommit(failedCommit, target),
		Branch:      cfg.Branch,
		Services:    allServices,
		Stacks:      reportStacks(cfg, stackNames(revertStacks)),
		HealthWatch: watchDuration,
		Duration:    time.Since(start),
		Err:         revertErr,
	})
	return Result{Reverted: true, RolledBackTo: target, HealthWatch: watchDuration, Err: revertErr, Notification: &embed, NotificationKey: key}
}

func degradedKey(failedCommit, target string) string {
	if failedCommit == preflightRecovery {
		return "degraded:preflight:" + target
	}
	return "degraded:" + failedCommit
}

func reportedCommit(failedCommit, target string) string {
	if failedCommit == preflightRecovery {
		return target
	}
	return failedCommit
}

func revertInterrupted(ctx context.Context, deps Deps, st *state.State, failedCommit, target string, stacks []compose.Stack, err error, start time.Time) Result {
	now := deps.Clock.Now()

	next := *st
	next.PendingRevert = true
	next.PendingCommit = failedCommit
	if names := stackNames(stacks); len(names) > 0 {
		next.PendingStacks = names
	}
	if next.PendingSince.IsZero() {
		next.PendingSince = now
	}
	if failedCommit != preflightRecovery {
		next.MarkFailed(failedCommit, now)
		next.LastAttemptCommit = failedCommit
	}
	next.LastAttemptAt = now
	next.LastResult = state.ResultFailedApply
	if saveErr := deps.State.Save(&next); saveErr != nil {
		deps.Log.Error("saving interrupted revert state", "err", saveErr)
		err = fmt.Errorf("%w (saving interrupted revert state also failed: %w)", err, saveErr)
	} else {
		*st = next
	}

	if ctx.Err() != nil {
		deps.Log.Warn("revert interrupted by shutdown, will resume on the next run", "target", target, "err", err)
		return Result{RolledBackTo: target, Err: err}
	}
	deps.Log.Error("revert did not finish, will resume on the next run", "target", target, "err", err)
	embed := notify.BuildEmbed(notify.Report{
		Outcome:  notify.OutcomeFailure,
		Title:    "ComposeLock: revert interrupted, will resume",
		Commit:   reportedCommit(failedCommit, target),
		Branch:   deps.Config.Branch,
		Stacks:   reportStacks(deps.Config, stackNames(stacks)),
		Duration: time.Since(start),
		Err:      err,
	})
	return Result{
		RolledBackTo:    target,
		Err:             err,
		Notification:    &embed,
		NotificationKey: "revert-interrupted:" + reportedCommit(failedCommit, target),
	}
}

func revertFailed(ctx context.Context, deps Deps, st *state.State, failedCommit, target string, stacks []compose.Stack, err error, start time.Time) Result {
	if ctx.Err() != nil || compose.IsInfraError(err) {
		return revertInterrupted(ctx, deps, st, failedCommit, target, stacks, err, start)
	}
	return degrade(deps, st, failedCommit, target, stackNames(stacks), err, start)
}

func degrade(deps Deps, st *state.State, failedCommit, target string, stacks []string, err error, start time.Time) Result {
	cfg := deps.Config
	now := deps.Clock.Now()
	next := *st
	next.LastResult = state.ResultDegraded
	next.LastAttemptAt = now
	next.PendingCommit = ""
	next.PendingSince = time.Time{}
	next.PendingAttempts = 0
	next.PendingStacks = nil
	next.PendingRevert = false
	if failedCommit != preflightRecovery {
		next.MarkFailed(failedCommit, now)
		next.LastAttemptCommit = failedCommit
	}
	saveErr := deps.State.Save(&next)
	if saveErr != nil {
		deps.Log.Error("saving degraded state", "err", saveErr)
		err = fmt.Errorf("%w (saving degraded state also failed: %w)", err, saveErr)
	} else {
		*st = next
	}
	embed := notify.BuildEmbed(notify.Report{
		Outcome:  notify.OutcomeFailure,
		Title:    "ComposeLock: DEGRADED, manual intervention required",
		Commit:   reportedCommit(failedCommit, target),
		Branch:   cfg.Branch,
		Stacks:   reportStacks(cfg, stacks),
		Duration: time.Since(start),
		Err:      err,
	})
	return Result{Degraded: true, RolledBackTo: target, Err: err, Notification: &embed, NotificationKey: degradedKey(failedCommit, target)}
}
