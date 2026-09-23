package reconcile

import (
	"errors"
	"testing"
	"time"

	"github.com/teyhouse/ComposeLock/internal/state"
)

func TestReconcilePausedAppliesNothingUntilThePauseEnds(t *testing.T) {
	st := state.New()
	st.Pause(time.Time{}.Add(time.Minute), time.Time{}.Add(time.Hour), "maintenance")
	g := gitChange("aaa111", "bbb222")
	compose := &fakeCompose{}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), st)
	deps.Clock = &fakeClock{now: time.Time{}.Add(30 * time.Minute)}

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if result.SkipReason != SkipPaused || compose.upCalls != 0 || g.head != "aaa111" {
		t.Fatalf("paused run: skip = %q, Up = %d, HEAD = %q, want a paused skip that touches nothing", result.SkipReason, compose.upCalls, g.head)
	}
	if len(store.State.History) != 0 {
		t.Errorf("a paused run was recorded in history: %+v", store.State.History)
	}

	deps.Clock = &fakeClock{now: time.Time{}.Add(2 * time.Hour)}
	result = Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if !result.Applied {
		t.Fatalf("after the pause ended: result = %+v, want the commit applied", result)
	}
}

func TestReconcileRecordsDeploysAndFailuresButNotIdleRuns(t *testing.T) {
	g := gitChange("aaa111", "bbb222")
	deps, store := testDeps(t, g, &fakeCompose{}, snapshots(healthySnapshot()), state.New())

	Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	g.remote, g.fetchErr = "ccc333", errors.New("network is unreachable")
	Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	h := store.State.History
	if len(h) != 2 || h[0].Result != state.ResultSuccess || h[0].Commit != "bbb222" || h[1].Result != state.ResultFailedGit || h[1].Error == "" {
		t.Errorf("history = %+v, want a success for bbb222 then a failed_git with its error, and nothing for the idle run", h)
	}
}
