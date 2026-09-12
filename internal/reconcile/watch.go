package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/health"
)

const maxConcurrentWatches = 16

type stackWatchOutcome struct {
	stack  compose.Stack
	result health.Result
	err    error
}

func watchStacks(ctx context.Context, deps Deps, stacks []compose.Stack, commit string) (health.Result, error) {
	if len(stacks) == 0 {
		return health.Result{Outcome: health.Healthy}, nil
	}

	cfg := deps.Config
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outcomes := make([]stackWatchOutcome, len(stacks))
	sem := make(chan struct{}, maxConcurrentWatches)

	var wg sync.WaitGroup
	for i, s := range stacks {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-watchCtx.Done():
				outcomes[i] = stackWatchOutcome{stack: s, err: watchCtx.Err()}
				return
			}
			opts := healthOptions(cfg, s.ProjectName, commit)
			res, err := health.Watch(watchCtx, deps.Health, deps.Clock, opts, deps.Log)
			outcomes[i] = stackWatchOutcome{stack: s, result: res, err: err}
			if err == nil && res.Outcome != health.Healthy {
				cancel()
			}
		})
	}
	wg.Wait()

	combined := health.Result{Outcome: health.Healthy}
	var reasons []string
	for _, o := range outcomes {
		if o.err != nil {
			if ctx.Err() == nil && errors.Is(o.err, context.Canceled) {
				deps.Log.Info("health watch: stopped early, another stack already failed", "project", o.stack.ProjectName)
				continue
			}
			return health.Result{}, fmt.Errorf("stack %s: %w", o.stack.ProjectName, o.err)
		}
		if o.result.Outcome != health.Healthy {
			combined.Outcome = health.Unhealthy
			reasons = append(reasons, o.stack.ProjectName+": "+o.result.Reason)
			for _, f := range o.result.Failures {
				combined.Failures = append(combined.Failures, o.stack.ProjectName+": "+f)
			}
		}
	}
	combined.Reason = strings.Join(reasons, "; ")
	return combined, nil
}
