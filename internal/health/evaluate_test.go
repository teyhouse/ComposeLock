package health

import "testing"

func TestEvaluatePreflightToleratesStarting(t *testing.T) {
	snap := Snapshot{Containers: []ContainerStatus{
		{ID: "c1", Service: "web", State: StateRunning, Health: HealthStarting},
	}}

	if healthy, reason := EvaluatePreflight(snap); !healthy {
		t.Errorf("EvaluatePreflight() = false (%s), want true", reason)
	}
	if healthy, _ := Evaluate(snap); healthy {
		t.Error("Evaluate() = true, want false: the final verdict must not accept a stuck starting container")
	}
}

func TestEvaluatePreflightStillRejectsRealFailures(t *testing.T) {
	tests := []struct {
		name      string
		container ContainerStatus
	}{
		{"unhealthy", ContainerStatus{Service: "web", State: StateRunning, Health: HealthUnhealthy}},
		{"exited nonzero", ContainerStatus{Service: "web", State: StateExited, ExitCode: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if healthy, _ := EvaluatePreflight(Snapshot{Containers: []ContainerStatus{tt.container}}); healthy {
				t.Error("EvaluatePreflight() = true, want false")
			}
		})
	}
}

func TestEvaluatePreflightAcceptsCompletedAndHealthy(t *testing.T) {
	snap := Snapshot{Containers: []ContainerStatus{
		{Service: "migrate", State: StateExited, ExitCode: 0},
		{Service: "web", State: StateRunning, Health: HealthHealthy},
		{Service: "cache", State: StateRunning, Health: HealthNone},
	}}
	if healthy, reason := EvaluatePreflight(snap); !healthy {
		t.Errorf("EvaluatePreflight() = false (%s), want true", reason)
	}
}

func TestEvaluateExemptsCompletedContainersFromTheHealthCheck(t *testing.T) {
	snap := Snapshot{Containers: []ContainerStatus{
		{ID: "c1", Service: "migrate", State: StateExited, ExitCode: 0, Health: HealthUnhealthy},
	}}

	healthy, reason := Evaluate(snap)
	if !healthy {
		t.Errorf("Evaluate() = false (%s), want a one-shot that exited 0 to pass both checks", reason)
	}
}

func TestChangedServicesNamesOnlyRecreatedContainers(t *testing.T) {
	before := Snapshot{Containers: []ContainerStatus{
		{ID: "c1", Service: "web"},
		{ID: "c2", Service: "db"},
	}}
	after := Snapshot{Containers: []ContainerStatus{
		{ID: "c9", Service: "web"},
		{ID: "c2", Service: "db"},
	}}

	got := ChangedServices(before, after)
	if len(got) != 1 || got[0] != "web" {
		t.Errorf("ChangedServices() = %v, want [web]: db kept its container", got)
	}
}

func TestChangedServicesNamesAddedAndRemoved(t *testing.T) {
	before := Snapshot{Containers: []ContainerStatus{{ID: "c1", Service: "old"}}}
	after := Snapshot{Containers: []ContainerStatus{{ID: "c2", Service: "new"}}}

	got := ChangedServices(before, after)
	if len(got) != 2 || got[0] != "new" || got[1] != "old" {
		t.Errorf("ChangedServices() = %v, want [new old]", got)
	}
}

func TestChangedServicesIsEmptyWhenNothingMoved(t *testing.T) {
	snap := Snapshot{Containers: []ContainerStatus{
		{ID: "c1", Service: "web"},
		{ID: "c2", Service: "db"},
	}}

	if got := ChangedServices(snap, snap); len(got) != 0 {
		t.Errorf("ChangedServices() = %v, want none", got)
	}
}

func TestChangedServicesHandlesScaledServices(t *testing.T) {
	before := Snapshot{Containers: []ContainerStatus{
		{ID: "c1", Service: "web"}, {ID: "c2", Service: "web"},
	}}
	after := Snapshot{Containers: []ContainerStatus{
		{ID: "c2", Service: "web"}, {ID: "c1", Service: "web"},
	}}

	if got := ChangedServices(before, after); len(got) != 0 {
		t.Errorf("ChangedServices() = %v, want none: the same two containers in another order", got)
	}
}
