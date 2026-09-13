package reconcile

import (
	"errors"
	"testing"

	"github.com/teyhouse/ComposeLock/internal/state"
)

func TestReconcileCrashRecoveryGivesUpAfterAttemptLimit(t *testing.T) {
	st := withHealthy("old111")
	st.PendingCommit = "pending333"
	compose := &fakeCompose{loadErr: errors.New("compose file is broken")}
	deps, store := testDeps(t, gitNoChange("old111"), compose, snapshots(healthySnapshot()), st)

	for attempt := 1; attempt <= maxRecoveryAttempts; attempt++ {
		result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
		if result.Degraded {
			t.Fatalf("attempt %d: went DEGRADED before the limit was reached", attempt)
		}
		if store.State.PendingAttempts != attempt {
			t.Fatalf("attempt %d: PendingAttempts = %d, want %d", attempt, store.State.PendingAttempts, attempt)
		}
	}

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if !result.Degraded {
		t.Fatalf("expected DEGRADED after %d failed recovery attempts, got %+v", maxRecoveryAttempts, result)
	}
	if store.State.LastResult != state.ResultDegraded {
		t.Errorf("LastResult = %q, want %q", store.State.LastResult, state.ResultDegraded)
	}

	blocked := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if !errors.Is(blocked.Err, errDegraded) {
		t.Errorf("expected the next run to be refused with errDegraded, got %v", blocked.Err)
	}
}

func TestReconcileCrashRecoveryResetsAttemptsOnSuccess(t *testing.T) {
	st := withHealthy("old111")
	st.PendingCommit = "pending333"
	st.PendingAttempts = 2
	deps, store := testDeps(t, gitNoChange("old111"), &fakeCompose{}, snapshots(healthySnapshot()), st)

	if result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps); result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if store.State.PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0 after a healthy apply", store.State.PendingAttempts)
	}
}

func TestReconcileCrashRecoveryForceResetsAttempts(t *testing.T) {
	st := withHealthy("old111")
	st.PendingCommit = "pending333"
	st.PendingAttempts = maxRecoveryAttempts
	deps, store := testDeps(t, gitNoChange("old111"), &fakeCompose{}, snapshots(healthySnapshot()), st)

	result := Reconcile(t.Context(), Options{Trigger: "cli", Force: true}, deps)
	if result.Degraded {
		t.Fatalf("--force should retry past the attempt limit, got %+v", result)
	}
	if store.State.LastHealthyCommit != "pending333" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "pending333")
	}
}

func TestReconcileFreshApplyResetsAttempts(t *testing.T) {
	st := withHealthy("old111")
	st.PendingAttempts = 2
	deps, store := testDeps(t, gitChange("old111", "new222"), &fakeCompose{}, snapshots(healthySnapshot()), st)

	if result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps); result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if store.State.PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0", store.State.PendingAttempts)
	}
}
