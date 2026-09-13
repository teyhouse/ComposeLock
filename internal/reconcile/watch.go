package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/health"
)

const maxConcurrentWatches = 16

type stackWatchOutcome struct {
	stack  compose.Stack
	result health.Result
	err    error
}

func watchStacks(ctx context.Context, deps Deps, stacks []compose.Stack, projects []*types.Project, commit string) (health.Result, map[string]health.Snapshot, error) {
	if len(stacks) == 0 {
		return health.Result{Outcome: health.Healthy}, nil, nil
	}

	cfg := deps.Config
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outcomes := make([]stackWatchOutcome, len(stacks))
	sem := make(chan struct{}, maxConcurrentWatches)

	var wg sync.WaitGroup
	for i, s := range stacks {
		select {
		case sem <- struct{}{}:
		case <-watchCtx.Done():
			outcomes[i] = stackWatchOutcome{stack: s, err: watchCtx.Err()}
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			opts := healthOptions(cfg, s.ProjectName, commit, expectsContainers(projects, i))
			res, err := health.Watch(watchCtx, deps.Health, deps.Clock, opts, deps.Log)
			outcomes[i] = stackWatchOutcome{stack: s, result: res, err: err}
			if err == nil && res.Outcome != health.Healthy {
				cancel()
			}
		})
	}
	wg.Wait()

	combined := health.Result{Outcome: health.Healthy}
	baselines := make(map[string]health.Snapshot, len(outcomes))
	var reasons, notEvaluated []string
	for _, o := range outcomes {
		if o.err != nil {
			if ctx.Err() == nil && errors.Is(o.err, context.Canceled) {
				deps.Log.Info("health watch: stopped early, another stack already failed", "project", o.stack.ProjectName)
				notEvaluated = append(notEvaluated, o.stack.ProjectName)
				continue
			}
			return health.Result{}, nil, fmt.Errorf("stack %s: %w", o.stack.ProjectName, o.err)
		}
		baselines[o.stack.ProjectName] = o.result.Baseline
		if o.result.Outcome != health.Healthy {
			combined.Outcome = health.Unhealthy
			reasons = append(reasons, o.stack.ProjectName+": "+o.result.Reason)
			for _, f := range o.result.Failures {
				combined.Failures = append(combined.Failures, o.stack.ProjectName+": "+f)
			}
		}
	}
	if len(notEvaluated) > 0 {
		reasons = append(reasons, "not evaluated, stopped early: "+strings.Join(notEvaluated, ", "))
	}
	combined.Reason = strings.Join(reasons, "; ")
	return combined, baselines, nil
}

func expectsContainers(projects []*types.Project, i int) bool {
	return i < len(projects) && projects[i] != nil && len(projects[i].Services) > 0
}
