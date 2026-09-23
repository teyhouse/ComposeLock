package reconcile

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
)

type alertSink struct {
	mu     sync.Mutex
	bodies []string
}

func (a *alertSink) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.bodies)
}

func monitorDeps(t *testing.T, snap health.Snapshotter) (Deps, *alertSink) {
	t.Helper()
	sink := &alertSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sink.mu.Lock()
		sink.bodies = append(sink.bodies, string(body))
		sink.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	deps, _ := testDeps(t, gitNoChange("abc123"), &fakeCompose{}, snap, withHealthy("abc123"))
	deps.Config.StateFile = t.Name()
	deps.Notifier = notify.New(srv.URL, slog.New(slog.DiscardHandler))
	return deps, sink
}

func TestMonitorAlertsOncePerIncidentAndNeverWhenHealthy(t *testing.T) {
	bad, good := unhealthySnapshot(), healthySnapshot()
	deps, sink := monitorDeps(t, snapshots(bad, bad, bad, bad, bad, good, good, bad, bad, bad))
	m := NewMonitor(deps)

	want := []int{0, 0, 1, 1, 1, 1, 1, 1, 1, 2}
	for i, w := range want {
		m.Check(t.Context())
		if got := sink.count(); got != w {
			t.Fatalf("after check %d: %d alerts, want %d", i+1, got, w)
		}
	}
	if !strings.Contains(sink.bodies[0], "Stack unhealthy: test-stack") {
		t.Errorf("alert body = %s, want the stack in the title", sink.bodies[0])
	}
}

func TestMonitorAlertsOnVanishedServiceAndSkipsPendingDeploys(t *testing.T) {
	both := health.Snapshot{Containers: []health.ContainerStatus{
		{ID: "c1", Service: "web", State: health.StateRunning},
		{ID: "c2", Service: "db", State: health.StateRunning},
	}}
	deps, sink := monitorDeps(t, snapshots(both, healthySnapshot()))
	m := NewMonitor(deps)
	for range 4 {
		m.Check(t.Context())
	}
	if sink.count() != 1 || !strings.Contains(sink.bodies[0], "no containers for: db") {
		t.Fatalf("alerts = %q, want one naming db", sink.bodies)
	}

	fake := snapshots(unhealthySnapshot())
	deps, sink = monitorDeps(t, fake)
	st, _ := deps.State.Load()
	st.PendingCommit = "def456"
	if err := deps.State.Save(st); err != nil {
		t.Fatal(err)
	}
	m = NewMonitor(deps)
	for range 5 {
		m.Check(t.Context())
	}
	if sink.count() != 0 || fake.call != 0 {
		t.Errorf("pending deploy: %d alerts, %d snapshots, want none", sink.count(), fake.call)
	}
}
