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
