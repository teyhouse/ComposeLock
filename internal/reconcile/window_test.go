package reconcile

import (
	"testing"
	"time"

	"github.com/teyhouse/ComposeLock/internal/state"
)

func TestReconcileHoldsNewCommitsOutsideTheDeployWindow(t *testing.T) {
	g := gitChange("aaa111", "bbb222")
	compose := &fakeCompose{}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"))
	deps.Config.DeployWindow = "02:00-05:00"
	deps.Clock = &fakeClock{now: time.Date(2026, 9, 23, 14, 0, 0, 0, time.UTC)}

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if result.SkipReason != SkipOutsideWindow || compose.upCalls != 0 || g.head != "aaa111" {
		t.Fatalf("outside the window: skip = %q, Up = %d, HEAD = %q, want the commit held", result.SkipReason, compose.upCalls, g.head)
	}
	if store.State.LastResult != state.Result("") {
		t.Errorf("a held commit changed the recorded result to %q", store.State.LastResult)
	}

	deps.Clock = &fakeClock{now: time.Date(2026, 9, 24, 2, 30, 0, 0, time.UTC)}
	if result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps); !result.Applied {
		t.Fatalf("inside the window: result = %+v, want the commit applied", result)
	}
}
