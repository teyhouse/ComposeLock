package reconcile

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/health"
)

type concurrencySnapshotter struct {
	mu        sync.Mutex
	inFlight  int
	maxSeen   int
	unhealthy map[string]bool
	delay     time.Duration
}

func (c *concurrencySnapshotter) Snapshot(_ context.Context, projectName string) (health.Snapshot, error) {
	c.mu.Lock()
	c.inFlight++
	c.maxSeen = max(c.maxSeen, c.inFlight)
	bad := c.unhealthy[projectName]
	c.mu.Unlock()

	time.Sleep(c.delay)

	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()

	if bad {
		return unhealthySnapshot(), nil
	}
	return healthySnapshot(), nil
}

func watchTestDeps(snap health.Snapshotter) Deps {
	return Deps{
		Config: &config.Config{
			ProjectName:               "test-stack",
			HealthWatchSeconds:        1,
			HealthPollIntervalSeconds: 1,
			HealthUnhealthyStreak:     1,
			HealthRestartTolerance:    1,
		},
		Health: snap,
		Clock:  health.RealClock{},
		Log:    slog.New(slog.DiscardHandler),
	}
}

func manyStacks(n int) []compose.Stack {
	stacks := make([]compose.Stack, n)
	for i := range stacks {
		stacks[i] = compose.Stack{ProjectName: "stack-" + string(rune('a'+i%26)) + string(rune('a'+i/26))}
	}
	return stacks
}

func TestWatchStacksRespectsConcurrencyLimit(t *testing.T) {
	stacks := manyStacks(maxConcurrentWatches + 4)
	snap := &concurrencySnapshotter{delay: 20 * time.Millisecond}
	deps := watchTestDeps(snap)

	result, err := watchStacks(t.Context(), deps, stacks, "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != health.Healthy {
		t.Errorf("Outcome = %v (%s), want healthy", result.Outcome, result.Reason)
	}

	snap.mu.Lock()
	seen := snap.maxSeen
	snap.mu.Unlock()
	if seen > maxConcurrentWatches {
		t.Errorf("observed %d concurrent watches, want <= %d", seen, maxConcurrentWatches)
	}
	if seen < 2 {
		t.Errorf("observed %d concurrent watches, expected the stacks to be watched in parallel", seen)
	}
}

func TestWatchStacksFailsOnAStackPastTheConcurrencyLimit(t *testing.T) {
	stacks := manyStacks(maxConcurrentWatches + 4)
	bad := stacks[len(stacks)-1].ProjectName
	deps := watchTestDeps(&concurrencySnapshotter{unhealthy: map[string]bool{bad: true}})

	result, err := watchStacks(t.Context(), deps, stacks, "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != health.Unhealthy {
		t.Fatalf("Outcome = %v, want unhealthy: a stack queued behind the limit must still be watched", result.Outcome)
	}
	if !strings.Contains(result.Reason, bad) {
		t.Errorf("Reason = %q, want it to name %q", result.Reason, bad)
	}
}

func TestWatchStacksStopsEarlyWhenCancelled(t *testing.T) {
	stacks := manyStacks(maxConcurrentWatches + 4)
	deps := watchTestDeps(&concurrencySnapshotter{delay: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := watchStacks(ctx, deps, stacks, "abc123"); err == nil {
		t.Fatal("expected an error when the parent context is already cancelled")
	}
}
