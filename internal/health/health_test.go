package health

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

// fakeSnapshotter returns snapshots[i] on the i-th call, clamped to the
// last entry once exhausted.
type fakeSnapshotter struct {
	snapshots []Snapshot
	call      int
}

func (f *fakeSnapshotter) Snapshot(_ context.Context, _ string) (Snapshot, error) {
	i := min(f.call, len(f.snapshots)-1)
	f.call++
	return f.snapshots[i], nil
}

// fakeClock advances instantly on Sleep — no real waiting — so tests run
// fast while still exercising the deadline/poll-count logic.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) bool {
	c.now = c.now.Add(d)
	return true
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func running(id, service string) ContainerStatus {
	return ContainerStatus{ID: id, Service: service, State: StateRunning, Health: ""}
}

func healthy(id, service string) ContainerStatus {
	return ContainerStatus{ID: id, Service: service, State: StateRunning, Health: HealthHealthy}
}

func baseOpts() Options {
	return Options{
		ProjectName:          "test-stack",
		WatchDuration:        30 * time.Second,
		PollInterval:         5 * time.Second,
		UnhealthyStreakLimit: 3,
		RestartTolerance:     1,
	}
}

func TestWatchFullWindowPasses(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web")}},
	}}
	result, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Healthy {
		t.Fatalf("Outcome = %v, want Healthy (reason: %s)", result.Outcome, result.Reason)
	}
}

func TestWatchContainerExitsFails(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web")}},
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateExited}}},
	}}
	result, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy", result.Outcome)
	}
}

func TestWatchRestartWithinToleranceContinues(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateRunning, RestartCount: 0}}},
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateRunning, RestartCount: 1}}},
	}}
	opts := baseOpts()
	opts.WatchDuration = 5 * time.Second // one poll only
	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Healthy {
		t.Fatalf("Outcome = %v, want Healthy (1 restart within tolerance 1)", result.Outcome)
	}
}

func TestWatchRestartBeyondToleranceFails(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateRunning, RestartCount: 0}}},
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateRunning, RestartCount: 2}}},
	}}
	result, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy (2 restarts > tolerance 1)", result.Outcome)
	}
}

func TestWatchUnhealthyStreakTripsFail(t *testing.T) {
	unhealthy := ContainerStatus{ID: "c1", Service: "web", State: StateRunning, Health: HealthUnhealthy}
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{unhealthy}},
		{Containers: []ContainerStatus{unhealthy}},
		{Containers: []ContainerStatus{unhealthy}},
	}}
	opts := baseOpts()
	opts.WatchDuration = 20 * time.Second // room for 3 polls at 5s interval
	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy after streak limit reached", result.Outcome)
	}
}

func TestWatchSingleUnhealthyBlipRecovers(t *testing.T) {
	unhealthy := ContainerStatus{ID: "c1", Service: "web", State: StateRunning, Health: HealthUnhealthy}
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{unhealthy}},
		{Containers: []ContainerStatus{healthy("c1", "web")}},
	}}
	opts := baseOpts()
	opts.WatchDuration = 10 * time.Second
	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Healthy {
		t.Fatalf("Outcome = %v, want Healthy after recovering from a single unhealthy blip", result.Outcome)
	}
}

func TestWatchStuckStartingForWholeWindowFails(t *testing.T) {
	starting := ContainerStatus{ID: "c1", Service: "web", State: StateRunning, Health: HealthStarting}
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{starting}},
	}}
	opts := baseOpts()
	opts.WatchDuration = 5 * time.Second
	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy (stuck starting for full window)", result.Outcome)
	}
}
