package reconcile

import (
	"errors"
	"testing"

	"github.com/teyhouse/ComposeLock/internal/health"
)

func startingSnapshot() health.Snapshot {
	return health.Snapshot{Containers: []health.ContainerStatus{
		{ID: "c1", Service: "web", State: health.StateRunning, Health: health.HealthStarting},
	}}
}

func TestReconcilePreflightToleratesStartingContainers(t *testing.T) {
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	deps, store := testDeps(t, g, compose, snapshots(startingSnapshot(), healthySnapshot()), withHealthy("old111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Reverted {
		t.Fatalf("a container inside its start_period must not trigger a revert, result = %+v", result)
	}
	if !result.Applied {
		t.Fatalf("expected the new commit to be applied, result = %+v", result)
	}
	if g.head != "new222" {
		t.Errorf("HEAD = %q, want %q", g.head, "new222")
	}
	if store.State.LastHealthyCommit != "new222" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "new222")
	}
}

func TestHealthWatchStillFailsOnStuckStartingContainer(t *testing.T) {
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	deps, _ := testDeps(t, g, compose, snapshots(
		healthySnapshot(),
		healthySnapshot(),
		startingSnapshot(),
		healthySnapshot(),
	), withHealthy("old111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("a container stuck in starting for the whole watch window must fail, result = %+v", result)
	}
}

type fakeLock struct {
	held      bool
	err       error
	lockCalls int
	unlocks   int
}

func (l *fakeLock) TryLock() (bool, error) {
	l.lockCalls++
	if l.err != nil {
		return false, l.err
	}
	return l.held, nil
}

func (l *fakeLock) Unlock() error {
	l.unlocks++
	return nil
}

func TestReconcileSkipsWhenAnotherProcessHoldsTheLock(t *testing.T) {
	compose := &fakeCompose{}
	deps, _ := testDeps(t, gitChange("old111", "new222"), compose, snapshots(healthySnapshot()), withHealthy("old111"))
	lock := &fakeLock{held: false}
	deps.Lock = lock

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if !result.Skipped {
		t.Errorf("expected Skipped = true, result = %+v", result)
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
	if lock.unlocks != 0 {
		t.Errorf("Unlock called %d times, want 0 (the lock was never acquired)", lock.unlocks)
	}
}

func TestReconcileReleasesTheLockAfterRunning(t *testing.T) {
	deps, _ := testDeps(t, gitChange("old111", "new222"), &fakeCompose{}, snapshots(healthySnapshot()), withHealthy("old111"))
	lock := &fakeLock{held: true}
	deps.Lock = lock

	if result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps); !result.Applied {
		t.Fatalf("expected the apply to run, result = %+v", result)
	}
	if lock.lockCalls != 1 || lock.unlocks != 1 {
		t.Errorf("lock calls = %d, unlocks = %d, want 1 and 1", lock.lockCalls, lock.unlocks)
	}
}

func TestReconcileReportsLockError(t *testing.T) {
	deps, _ := testDeps(t, gitChange("old111", "new222"), &fakeCompose{}, snapshots(healthySnapshot()), withHealthy("old111"))
	deps.Lock = &fakeLock{err: errors.New("permission denied")}

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err == nil {
		t.Fatal("expected an error when the lock cannot be taken")
	}
}
