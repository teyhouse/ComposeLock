package reconcile

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/teyhouse/ComposeLock/internal/heartbeat"
)

func TestHeartbeatPingsAfterRealRunsOnly(t *testing.T) {
	var pings atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pings.Add(1)
	}))
	defer srv.Close()

	deps, _ := testDeps(t, gitNoChange("abc123"), &fakeCompose{}, snapshots(healthySnapshot()), withHealthy("abc123"))
	deps.Config.StateFile = t.Name()
	deps.Heartbeat = heartbeat.New(srv.URL, slog.New(slog.DiscardHandler))

	Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	Reconcile(t.Context(), Options{Trigger: "cli", DryRun: true}, deps)
	if got := pings.Load(); got != 1 {
		t.Fatalf("after a run and a dry run: %d pings, want 1", got)
	}

	m := NewMonitor(deps)
	m.Check(t.Context())
	deps.activity().Add(1)
	m.Check(t.Context())
	deps.activity().Add(1)
	if got := pings.Load(); got != 2 {
		t.Errorf("after an idle and a busy monitor check: %d pings, want 2", got)
	}
}
