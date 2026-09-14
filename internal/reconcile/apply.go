package reconcile

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

const restoreCheckoutTimeout = 30 * time.Second

const maxPreflightBlocks = 3

func runNormal(ctx context.Context, opts Options, deps Deps, st *state.State, start time.Time) Result {
	cfg := deps.Config
	retryDelay := time.Duration(cfg.RetryDelaySeconds) * time.Second

	gitResult, err := git.Fetch(ctx, deps.Git, cfg.RetryAttempts, retryDelay, deps.Log)
	if err != nil {
		deps.Log.Error("git sync failed", "err", err)
		if ctx.Err() != nil {
			return Result{Err: err}
		}
		recordOutcome(deps, st, state.ResultFailedGit, "")
		embed := notify.BuildEmbed(notify.Report{
			Outcome: notify.OutcomeFailure,
			Title:   "ComposeLock: git sync failed",
			Branch:  cfg.Branch,
			Err:     err,
		})
		return Result{Err: err, Notification: &embed, NotificationKey: "git-sync"}
	}

	result := Result{
		Changed:      gitResult.Changed,
		OldCommit:    gitResult.OldCommit,
		NewCommit:    gitResult.NewCommit,
		ChangedFiles: gitResult.ChangedFiles,
	}
	if !gitResult.Changed {
		if !checkoutDrifted(st, gitResult.OldCommit) {
			deps.Log.Info("nothing to do", "commit", gitResult.OldCommit)
			return result
		}
		deps.Log.Warn("checkout does not match the expected commit, re-applying",
			"head", gitResult.OldCommit, "expected", st.ExpectedCheckout(), "last_healthy_commit", st.LastHealthyCommit)
		result.Changed = true
		result.ChangedFiles = nil
	} else if st.LastHealthyCommit != "" && !relevantChange(ctx, deps, gitResult.ChangedFiles) {
		deps.Log.Info("compose file(s) unchanged, advancing checkout without applying",
			"commit", gitResult.NewCommit, "changed_files", gitResult.ChangedFiles)
		result.Skipped = true
		return advanceCheckout(ctx, deps, st, result)
	}

	// A revert moves the local checkout back to last_healthy_commit but
	// never moves origin/<branch>, so without this guard the next
	// Reconcile would see local != remote again and re-apply the same bad
	// commit forever.
	if st.IsKnownBad(gitResult.NewCommit) && !opts.Force {
		deps.Log.Info("skipping known-bad commit", "commit", gitResult.NewCommit)
		recordOutcome(deps, st, state.ResultSkippedKnownBad, gitResult.NewCommit)
		result.Skipped = true
		return result
	}

	previousStacks, err := previousStacksAt(deps)
	if err != nil {
		if ctx.Err() != nil {
			result.Err = fmt.Errorf("discovering compose stacks: %w", err)
			return result
		}
		deps.Log.Warn("discovering compose stacks at the current checkout failed, continuing with the last healthy stack set so a repairing commit can still be checked out",
			"commit", gitResult.OldCommit, "err", err)
		previousStacks = deployedStacks(st, nil)
	}

	live, err := evaluateLive(ctx, deps, previousStacks)
	if err != nil {
		deps.Log.Error("pre-flight snapshot failed", "err", err)
		result.Err = fmt.Errorf("pre-flight snapshot: %w", err)
		if ctx.Err() != nil {
			return result
		}
		recordOutcome(deps, st, state.ResultFailedPreflight, gitResult.NewCommit)
		embed := notify.BuildEmbed(notify.Report{
			Outcome: notify.OutcomeFailure,
			Title:   "ComposeLock: pre-flight check failed",
			Commit:  gitResult.NewCommit,
			Branch:  cfg.Branch,
			Err:     result.Err,
		})
		result.Notification = &embed
		result.NotificationKey = "preflight:" + gitResult.NewCommit
		return result
	}

	if opts.DryRun {
		deps.Log.Info("dry run: would apply", "old_commit", gitResult.OldCommit, "new_commit", gitResult.NewCommit, "live_healthy", live.healthy, "gate_reason", live.reason)
		if !live.healthy {
			result.Skipped = true
			result.SkipReason = SkipPreflightUnhealthy
			result.Err = fmt.Errorf("pre-flight gate would block this commit: %s", live.reason)
		}
		return result
	}

	if !live.healthy {
		return preflightBlocked(ctx, opts, deps, st, result, previousStacks, live.reason, start)
	}

	return applyAndWatch(ctx, deps, st, result, deployedStacks(st, previousStacks), nil, live.snapshots, start)
}

func preflightBlocked(ctx context.Context, opts Options, deps Deps, st *state.State, result Result, previousStacks []compose.Stack, reason string, start time.Time) Result {
	deps.Log.Warn("pre-flight gate: live stack unhealthy, not applying", "reason", reason)
	if opts.Force {
		st.PreflightBlocks = 0
	}
	if st.PreflightBlocks >= maxPreflightBlocks {
		deps.Log.Error("pre-flight gate: blocked repeatedly, DEGRADED", "reason", reason, "blocks", st.PreflightBlocks)
		degraded := degrade(deps, st, preflightRecovery, st.LastHealthyCommit, stackNames(previousStacks),
			fmt.Errorf("pre-flight gate blocked %d consecutive runs (%s): manual intervention required", st.PreflightBlocks, reason),
			start)
		degraded.Changed = result.Changed
		degraded.OldCommit = result.OldCommit
		degraded.NewCommit = result.NewCommit
		degraded.ChangedFiles = result.ChangedFiles
		return degraded
	}

	next := *st
	next.PreflightBlocks++
	if err := deps.State.Save(&next); err != nil {
		deps.Log.Error("saving pre-flight block counter", "err", err)
	} else {
		*st = next
	}

	revertResult := doRevert(ctx, deps, st, previousStacks, preflightRecovery, "pre-flight: live stack unhealthy: "+reason, start)
	revertResult.Changed = result.Changed
	revertResult.OldCommit = result.OldCommit
	revertResult.NewCommit = result.NewCommit
	revertResult.ChangedFiles = result.ChangedFiles
	return revertResult
}

func checkoutDrifted(st *state.State, head string) bool {
	return head != "" && head != st.ExpectedCheckout()
}

func advanceCheckout(ctx context.Context, deps Deps, st *state.State, result Result) Result {
	if err := deps.Git.Checkout(ctx, result.NewCommit); err != nil {
		return abortBeforeUp(ctx, deps, st, result, "checkout failed", err)
	}
	if err := recordCheckout(deps, st, result.NewCommit); err != nil {
		result.Err = err
	}
	return result
}

func recordCheckout(deps Deps, st *state.State, commit string) error {
	next := *st
	next.LastCheckoutCommit = commit
	next.LastAttemptCommit = commit
	next.LastAttemptAt = deps.Clock.Now()
	next.LastResult = state.ResultSuccess
	if err := deps.State.Save(&next); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	*st = next
	return nil
}

func previousStacksAt(deps Deps) ([]compose.Stack, error) {
	return stacksAllowingEmpty(deps)
}

func applyAndWatch(ctx context.Context, deps Deps, st *state.State, result Result, previousStacks []compose.Stack, pendingStacks []string, before map[string]health.Snapshot, start time.Time) Result {
	cfg := deps.Config
	commit := result.NewCommit

	if err := deps.Git.Checkout(ctx, commit); err != nil {
		return abortBeforeUp(ctx, deps, st, result, "checkout failed", err)
	}

	stacks, err := stacksAllowingEmpty(deps)
	if err != nil {
		return abortApply(ctx, deps, st, result, "discovering compose stacks failed", err)
	}
	vanished := vanishedStacks(previousStacks, stacks)

	changedStacks := stacks
	switch {
	case st.LastHealthyCommit == "":
		deps.Log.Info("no healthy deployment recorded yet, applying every stack", "commit", commit)
	case result.ChangedFiles != nil:
		changedStacks = filterChanged(cfg.RepoPath, stacks, previousStacks, result.ChangedFiles)
	case len(pendingStacks) > 0:
		changedStacks = restrictToNames(stacks, pendingStacks)
	}
	if len(changedStacks) == 0 {
		if err := tearDownStacks(ctx, deps, vanished, "stack removed from compose_dir"); err != nil {
			result.Err = err
			return result
		}
		deps.Log.Info("no compose stack changed, nothing to apply", "commit", commit)
		result.Skipped = true
		if err := promoteHealthy(deps, st, commit, stackNames(stacks)); err != nil {
			result.Err = err
		}
		return result
	}

	projects := make([]*types.Project, len(changedStacks))
	for i, s := range changedStacks {
		project, err := deps.Compose.LoadProject(ctx, s.Files, s.ProjectName)
		if err != nil {
			return abortApply(ctx, deps, st, result, "loading compose project failed", fmt.Errorf("loading project %s: %w", s.ProjectName, err))
		}
		if err := compose.CheckEnvFiles(project); err != nil {
			return abortApply(ctx, deps, st, result, "env_file missing", err)
		}
		projects[i] = project
	}

	// Persisted before touching Docker so a crash here is recoverable via
	// pending_commit on the next run.
	now := deps.Clock.Now()
	next := *st
	if next.PendingCommit != commit {
		next.PendingAttempts = 0
	}
	next.PendingCommit = commit
	next.PendingSince = now
	next.PendingStacks = stackNames(changedStacks)
	next.LastCheckoutCommit = commit
	next.LastAttemptCommit = commit
	next.LastAttemptAt = now
	next.ForgetFailed(commit)
	if err := deps.State.Save(&next); err != nil {
		return abortBeforeUp(ctx, deps, st, result, "saving state failed", fmt.Errorf("saving state: %w", err))
	}
	*st = next

	var allServices []string
	for i, s := range changedStacks {
		if err := deps.Compose.Up(ctx, projects[i]); err != nil {
			if ctx.Err() != nil {
				result.Err = fmt.Errorf("compose up interrupted: %w", err)
				return result
			}
			if compose.IsInfraError(err) {
				return applyInterrupted(deps, st, result, s.ProjectName, err)
			}
			deps.Log.Error("compose up failed, reverting", "stack", s.ProjectName, "err", err)
			return failAndRevert(ctx, deps, st, changedStacks, result, fmt.Sprintf("applying %s (%s) failed: %v", shortCommit(commit), s.ProjectName, err), start)
		}
		allServices = append(allServices, serviceNames(projects[i])...)
	}

	result.Applied = true
	result.Services = allServices
	result.Stacks = stackNames(changedStacks)

	watchResult, baselines, err := watchStacks(ctx, deps, changedStacks, projects, commit)
	if err != nil {
		result.Err = fmt.Errorf("health watch: %w", err)
		return result
	}
	result.HealthWatch = time.Duration(cfg.HealthWatchSeconds) * time.Second

	if watchResult.Outcome == health.Healthy {
		if err := tearDownStacks(ctx, deps, vanished, "stack removed from compose_dir"); err != nil {
			result.Err = err
			return result
		}
		if err := promoteHealthy(deps, st, commit, stackNames(stacks)); err != nil {
			result.Err = err
			return result
		}
		updated, restarted, updatedKnown := updatedServices(cfg, before, baselines, changedStacks)
		result.Updated = updated
		result.Restarted = restarted
		embed := notify.BuildEmbed(notify.Report{
			Outcome:      notify.OutcomeSuccess,
			Title:        "ComposeLock: deployed successfully",
			Commit:       commit,
			Branch:       cfg.Branch,
			Services:     result.Services,
			Updated:      updated,
			UpdatedKnown: updatedKnown,
			Restarted:    restarted,
			Stacks:       reportStacks(cfg, result.Stacks),
			HealthWatch:  result.HealthWatch,
			Duration:     time.Since(start),
		})
		result.Notification = &embed
		return result
	}

	deps.Log.Warn("health watch failed, reverting", "reason", watchResult.Reason)
	return failAndRevert(ctx, deps, st, changedStacks, result, fmt.Sprintf("applied %s failed health watch (%s)", shortCommit(commit), watchResult.Reason), start)
}

func failAndRevert(ctx context.Context, deps Deps, st *state.State, stacks []compose.Stack, result Result, reason string, start time.Time) Result {
	st.MarkFailed(result.NewCommit, deps.Clock.Now())
	st.LastResult = state.ResultFailedApply
	if err := deps.State.Save(st); err != nil {
		deps.Log.Error("saving state before revert", "err", err)
	}

	revertResult := doRevert(ctx, deps, st, stacks, result.NewCommit, reason, start)
	result.Reverted = revertResult.Reverted
	result.Degraded = revertResult.Degraded
	result.RolledBackTo = revertResult.RolledBackTo
	result.Err = revertResult.Err
	result.Notification = revertResult.Notification
	result.NotificationKey = revertResult.NotificationKey
	result.HealthWatch += revertResult.HealthWatch
	return result
}

func abortApply(ctx context.Context, deps Deps, st *state.State, result Result, title string, err error) Result {
	if compose.IsInfraError(err) {
		deps.Log.Warn("aborting on an infrastructure error, the commit stays eligible for a retry", "err", err)
		return abortBeforeUp(ctx, deps, st, result, title, err)
	}
	st.MarkFailed(result.NewCommit, deps.Clock.Now())
	return abortBeforeUp(ctx, deps, st, result, title, err)
}

func applyInterrupted(deps Deps, st *state.State, result Result, projectName string, err error) Result {
	result.Err = fmt.Errorf("compose up of %s hit an infrastructure error: %w", projectName, err)
	deps.Log.Error("compose up failed on infrastructure, leaving the apply pending for the next run",
		"stack", projectName, "commit", result.NewCommit, "err", err)
	recordOutcome(deps, st, state.ResultFailedApply, result.NewCommit)
	embed := notify.BuildEmbed(notify.Report{
		Outcome: notify.OutcomeFailure,
		Title:   "ComposeLock: infrastructure error during apply, will retry",
		Commit:  result.NewCommit,
		Branch:  deps.Config.Branch,
		Err:     result.Err,
	})
	result.Notification = &embed
	result.NotificationKey = "infra:" + result.NewCommit
	return result
}

func abortBeforeUp(ctx context.Context, deps Deps, st *state.State, result Result, title string, err error) Result {
	deps.Log.Error(title, "err", err)
	restoreErr := restoreCheckout(ctx, deps, result.OldCommit)
	result.Err = errors.Join(err, restoreErr)
	if ctx.Err() != nil {
		return result
	}
	recordOutcome(deps, st, state.ResultFailedApply, result.NewCommit)
	embed := notify.BuildEmbed(notify.Report{
		Outcome: notify.OutcomeFailure,
		Title:   "ComposeLock: " + title,
		Commit:  result.NewCommit,
		Branch:  deps.Config.Branch,
		Err:     result.Err,
	})
	result.Notification = &embed
	result.NotificationKey = "abort:" + title + ":" + result.NewCommit
	return result
}

func restoreCheckout(ctx context.Context, deps Deps, commit string) error {
	if commit == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreCheckoutTimeout)
	defer cancel()
	if err := deps.Git.Checkout(ctx, commit); err != nil {
		deps.Log.Error("restoring previous checkout failed", "commit", commit, "err", err)
		return fmt.Errorf("restoring checkout to %s: %w", shortCommit(commit), err)
	}
	return nil
}

func relevantChange(ctx context.Context, deps Deps, changedFiles []string) bool {
	cfg := deps.Config
	if cfg.ComposeDir != "" {
		return composeDirRelevant(cfg.RepoPath, cfg.ComposeDir, changedFiles)
	}
	return composeFileRelevant(ctx, deps, changedFiles)
}

func composeFileRelevant(ctx context.Context, deps Deps, changedFiles []string) bool {
	cfg := deps.Config
	if composeFileChanged(cfg.RepoPath, cfg.ComposeFile, changedFiles) {
		return true
	}
	project, err := deps.Compose.LoadProject(ctx, []string{cfg.ComposeFile}, cfg.ProjectName)
	if err != nil {
		deps.Log.Warn("loading the compose project to weigh a change failed, treating the commit as relevant", "err", err)
		return true
	}
	inputs, complete := compose.ProjectInputs(project)
	if !complete {
		deps.Log.Info("compose project has inputs that cannot be resolved without applying it, treating the commit as relevant", "inputs", inputs)
		return true
	}
	deps.Log.Debug("weighing changed files against the project's inputs", "inputs", inputs)
	return dependencyChanged(cfg.RepoPath, inputs, changedFiles)
}

func dependencyChanged(repoPath string, inputs []string, changedFiles []string) bool {
	for _, f := range changedFiles {
		abs := resolveUnderRepo(repoPath, f)
		for _, in := range inputs {
			if abs == in || pathUnder(in, abs) {
				return true
			}
		}
	}
	return false
}

func composeFileChanged(repoPath, composeFile string, changedFiles []string) bool {
	return stackFilesChanged(repoPath, []string{composeFile}, changedFiles)
}

func stackFilesChanged(repoPath string, files []string, changedFiles []string) bool {
	targets := make(map[string]struct{}, len(files))
	for _, f := range files {
		targets[resolveUnderRepo(repoPath, f)] = struct{}{}
	}
	for _, f := range changedFiles {
		if _, ok := targets[resolveUnderRepo(repoPath, f)]; ok {
			return true
		}
	}
	return false
}

func composeDirRelevant(repoPath, composeDir string, changedFiles []string) bool {
	dir := resolveUnderRepo(repoPath, composeDir)
	for _, f := range changedFiles {
		if pathUnder(dir, resolveUnderRepo(repoPath, f)) {
			return true
		}
	}
	return false
}

func pathUnder(dir, abs string) bool {
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveUnderRepo(repoPath, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(repoPath, p)
	}
	return filepath.Clean(p)
}
