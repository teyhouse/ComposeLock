package health

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type stalledClock struct {
	now     time.Time
	step    time.Duration
	stalled bool
}

func (c *stalledClock) Now() time.Time {
	if !c.stalled {
		c.stalled = true
		return c.now
	}
	c.now = c.now.Add(c.step)
	return c.now
}

func (c *stalledClock) Sleep(_ context.Context, d time.Duration) bool {
	c.now = c.now.Add(d)
	return true
}

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
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateExited, ExitCode: 137}}},
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

func TestWatchCompletedOneShotContainerPasses(t *testing.T) {
	migrate := ContainerStatus{ID: "c2", Service: "migrate", State: StateExited, ExitCode: 0}
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web"), migrate}},
	}}
	result, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Healthy {
		t.Fatalf("Outcome = %v, want Healthy (reason: %s)", result.Outcome, result.Reason)
	}
}

func TestWatchFailedOneShotContainerFails(t *testing.T) {
	migrate := ContainerStatus{ID: "c2", Service: "migrate", State: StateExited, ExitCode: 1}
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web"), migrate}},
	}}
	result, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy", result.Outcome)
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name      string
		container ContainerStatus
		want      bool
	}{
		{"running", running("c1", "web"), true},
		{"completed one-shot", ContainerStatus{ID: "c1", Service: "migrate", State: StateExited, ExitCode: 0}, true},
		{"failed one-shot", ContainerStatus{ID: "c1", Service: "migrate", State: StateExited, ExitCode: 1}, false},
		{"restarting", ContainerStatus{ID: "c1", Service: "web", State: "restarting"}, false},
		{"unhealthy", ContainerStatus{ID: "c1", Service: "web", State: StateRunning, Health: HealthUnhealthy}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := Evaluate(Snapshot{Containers: []ContainerStatus{tt.container}})
			if got != tt.want {
				t.Errorf("Evaluate() = %v (%s), want %v", got, reason, tt.want)
			}
		})
	}
}

func TestOutcomeZeroValueIsNotHealthy(t *testing.T) {
	var result Result
	if result.Outcome == Healthy {
		t.Error("zero-value Outcome must not read as Healthy")
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

func TestWatchEmptySnapshotFailsWhenContainersAreExpected(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{{}}}
	opts := baseOpts()
	opts.ExpectContainers = true

	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy: a project with services and no containers is not a healthy deploy", result.Outcome)
	}
}

func TestWatchEmptySnapshotStaysHealthyForAServicelessProject(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{{}}}

	result, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Healthy {
		t.Fatalf("Outcome = %v, want Healthy", result.Outcome)
	}
}

func TestWatchContainersDisappearingMidWindowFails(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web"), running("c2", "db")}},
		{Containers: []ContainerStatus{running("c1", "web")}},
	}}
	opts := baseOpts()
	opts.ExpectContainers = true

	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy when a service's container vanishes", result.Outcome)
	}
}

func TestWatchRecreatedContainerKeepsItsRestartBudget(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateRunning, RestartCount: 3}}},
		{Containers: []ContainerStatus{{ID: "c2", Service: "web", State: StateRunning, RestartCount: 5}}},
	}}
	opts := baseOpts()
	opts.RestartTolerance = 1

	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy: a new container ID must not reset the restart baseline", result.Outcome)
	}
}

func TestWatchAlternatingStartingAndUnhealthyReachesTheStreakLimit(t *testing.T) {
	unhealthyC := ContainerStatus{ID: "c1", Service: "web", State: StateRunning, Health: HealthUnhealthy}
	startingC := ContainerStatus{ID: "c1", Service: "web", State: StateRunning, Health: HealthStarting}
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web")}},
		{Containers: []ContainerStatus{unhealthyC}},
		{Containers: []ContainerStatus{startingC}},
		{Containers: []ContainerStatus{unhealthyC}},
		{Containers: []ContainerStatus{startingC}},
		{Containers: []ContainerStatus{unhealthyC}},
	}}
	opts := baseOpts()
	opts.UnhealthyStreakLimit = 3

	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want Unhealthy: a starting poll must not clear the unhealthy streak", result.Outcome)
	}
}

func TestWatchDisabledSkipsTheBaselineSnapshot(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{{}}}
	opts := baseOpts()
	opts.WatchDuration = 0

	result, err := Watch(t.Context(), snap, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if result.Outcome != Healthy {
		t.Fatalf("Outcome = %v, want Healthy", result.Outcome)
	}
	if snap.call != 0 {
		t.Errorf("took %d snapshots, want 0 when the watch is disabled", snap.call)
	}
}

func TestWatchRestartBaselineIsPerContainerNotPerService(t *testing.T) {
	lagging := ContainerStatus{ID: "c1", Service: "web", State: StateRunning, RestartCount: 0}
	settled := ContainerStatus{ID: "c2", Service: "web", State: StateRunning, RestartCount: 5}
	crashing := ContainerStatus{ID: "c1", Service: "web", State: StateRunning, RestartCount: 3}

	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{lagging, settled}},
		{Containers: []ContainerStatus{crashing, settled}},
	}}

	res, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Outcome != Unhealthy {
		t.Errorf("Outcome = %v, want unhealthy: a replica that restarted 3 times must not hide behind a sibling's higher count", res.Outcome)
	}
}

func TestWatchLosingOneReplicaFails(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web"), running("c2", "web")}},
		{Containers: []ContainerStatus{running("c1", "web")}},
	}}

	res, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want unhealthy when a replica disappears", res.Outcome)
	}
	if !strings.Contains(res.Reason, "1 of 2 replicas") {
		t.Errorf("Reason = %q, want the replica counts", res.Reason)
	}
}

func TestWatchScalingUpMidWindowStaysHealthy(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web")}},
		{Containers: []ContainerStatus{running("c1", "web"), running("c2", "web")}},
	}}

	res, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Outcome != Healthy {
		t.Errorf("Outcome = %v (%s), want healthy: extra replicas are not a failure", res.Outcome, res.Reason)
	}
}

func TestWatchDeadContainerFailsOnTheFirstPoll(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web")}},
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateDead}}},
	}}
	clock := &fakeClock{}

	res, err := Watch(t.Context(), snap, clock, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want unhealthy", res.Outcome)
	}
	if snap.call != 2 {
		t.Errorf("snapshot calls = %d, want the watch to stop after the first bad poll instead of burning the window", snap.call)
	}
}

func TestWatchContainerStuckInCreatedFailsAtTheStreakLimit(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web")}},
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateCreated}}},
	}}

	res, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Outcome != Unhealthy {
		t.Fatalf("Outcome = %v, want unhealthy", res.Outcome)
	}
	if snap.call != 1+baseOpts().UnhealthyStreakLimit {
		t.Errorf("snapshot calls = %d, want the wedge to fail after %d polls", snap.call, baseOpts().UnhealthyStreakLimit)
	}
}

func TestWatchAlwaysPollsOnceEvenIfTheWindowAlreadyElapsed(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateRunning, Health: HealthStarting}}},
		{Containers: []ContainerStatus{healthy("c1", "web")}},
	}}
	opts := baseOpts()
	opts.WatchDuration = time.Nanosecond
	opts.PollInterval = time.Second

	res, err := Watch(t.Context(), snap, &stalledClock{step: time.Second}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Outcome != Healthy {
		t.Errorf("Outcome = %v (%s), want healthy: judging the post-Up baseline, where a container is still starting, is a false failure", res.Outcome, res.Reason)
	}
	if snap.call != 2 {
		t.Errorf("snapshot calls = %d, want the baseline plus one real poll", snap.call)
	}
}

func TestWatchStillFailsOnTheOnlyPollOfAnElapsedWindow(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{running("c1", "web")}},
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateExited, ExitCode: 1}}},
	}}
	opts := baseOpts()
	opts.WatchDuration = time.Nanosecond
	opts.PollInterval = time.Second

	res, err := Watch(t.Context(), snap, &stalledClock{step: time.Second}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Outcome != Unhealthy {
		t.Errorf("Outcome = %v, want unhealthy", res.Outcome)
	}
}

func TestWatchRestartingAtTheFinalPollWithinToleranceStaysHealthy(t *testing.T) {
	snap := &fakeSnapshotter{snapshots: []Snapshot{
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateRunning, RestartCount: 0}}},
		{Containers: []ContainerStatus{{ID: "c1", Service: "web", State: StateRestarting, RestartCount: 1}}},
	}}

	res, err := Watch(t.Context(), snap, &fakeClock{}, baseOpts(), testLog())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Outcome != Healthy {
		t.Errorf("Outcome = %v (%s), want healthy: a restart within tolerance is not a failed deploy", res.Outcome, res.Reason)
	}
}

func TestRestartedServicesNamesInPlaceRestarts(t *testing.T) {
	before := Snapshot{Containers: []ContainerStatus{
		{ID: "c1", Service: "web", RestartCount: 2},
		{ID: "c2", Service: "db", RestartCount: 0},
	}}
	after := Snapshot{Containers: []ContainerStatus{
		{ID: "c1", Service: "web", RestartCount: 3},
		{ID: "c2", Service: "db", RestartCount: 0},
	}}

	if got := RestartedServices(before, after); len(got) != 1 || got[0] != "web" {
		t.Errorf("RestartedServices = %v, want [web]", got)
	}
	if got := ChangedServices(before, after); len(got) != 0 {
		t.Errorf("ChangedServices = %v, want empty: the container kept its id", got)
	}
}
