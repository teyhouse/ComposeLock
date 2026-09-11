package health

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	HealthNone      = "none"
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

const (
	StateRunning = "running"
	StateExited  = "exited"
)

type ContainerStatus struct {
	ID           string
	Service      string
	State        string // "running", "exited", ...
	Health       string // "", "none", "starting", "healthy", "unhealthy"
	RestartCount int
	ExitCode     int
}

func (c ContainerStatus) Completed() bool {
	return c.State == StateExited && c.ExitCode == 0
}

type Snapshot struct {
	Containers []ContainerStatus
	Taken      time.Time
}

type Snapshotter interface {
	Snapshot(ctx context.Context, projectName string) (Snapshot, error)
}

type Clock interface {
	Now() time.Time
	// Sleep returns false early if ctx is cancelled before d elapses.
	Sleep(ctx context.Context, d time.Duration) bool
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) Sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type Outcome int

const (
	Unknown Outcome = iota
	Healthy
	Unhealthy
)

func (o Outcome) String() string {
	switch o {
	case Healthy:
		return "healthy"
	case Unhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

type Options struct {
	ProjectName          string
	WatchDuration        time.Duration
	PollInterval         time.Duration
	UnhealthyStreakLimit int
	RestartTolerance     int
}

type Result struct {
	Outcome  Outcome
	Reason   string
	Failures []string
}

func Watch(ctx context.Context, snap Snapshotter, clock Clock, opts Options, log *slog.Logger) (Result, error) {
	baseline, err := snap.Snapshot(ctx, opts.ProjectName)
	if err != nil {
		return Result{}, fmt.Errorf("baseline snapshot: %w", err)
	}
	baselineRestarts := make(map[string]int, len(baseline.Containers))
	for _, c := range baseline.Containers {
		baselineRestarts[c.ID] = c.RestartCount
	}

	deadline := clock.Now().Add(opts.WatchDuration)
	result := Result{}
	unhealthyStreak := 0
	var last Snapshot

	for clock.Now().Before(deadline) {
		if !clock.Sleep(ctx, opts.PollInterval) {
			return Result{}, ctx.Err()
		}

		cur, err := snap.Snapshot(ctx, opts.ProjectName)
		if err != nil {
			return Result{}, fmt.Errorf("health snapshot: %w", err)
		}
		last = cur

		anyUnhealthy := false
		for _, c := range cur.Containers {
			if c.State == StateExited && !c.Completed() {
				result.Reason = fmt.Sprintf("container exited: %s (exit code %d)", c.Service, c.ExitCode)
				result.Failures = append(result.Failures, result.Reason)
				log.Warn("health watch: container exited", "service", c.Service, "exit_code", c.ExitCode)
				result.Outcome = Unhealthy
				return result, nil
			}
			if c.RestartCount > baselineRestarts[c.ID]+opts.RestartTolerance {
				result.Reason = fmt.Sprintf("restarted beyond tolerance: %s (restarts=%d, tolerance=%d)", c.Service, c.RestartCount-baselineRestarts[c.ID], opts.RestartTolerance)
				result.Failures = append(result.Failures, result.Reason)
				log.Warn("health watch: restart tolerance exceeded", "service", c.Service, "restarts", c.RestartCount-baselineRestarts[c.ID])
				result.Outcome = Unhealthy
				return result, nil
			}
			if c.Health == HealthUnhealthy {
				anyUnhealthy = true
			}
		}

		if anyUnhealthy {
			unhealthyStreak++
			if unhealthyStreak >= opts.UnhealthyStreakLimit {
				result.Reason = "healthcheck unhealthy"
				result.Failures = append(result.Failures, result.Reason)
				log.Warn("health watch: unhealthy streak limit reached", "streak", unhealthyStreak)
				result.Outcome = Unhealthy
				return result, nil
			}
		} else {
			unhealthyStreak = 0
		}
	}

	// A container stuck in "starting" for the whole window never triggers
	// the loop's per-poll checks above, so it must fail here explicitly.
	healthy, reason := Evaluate(last)
	if !healthy {
		result.Outcome = Unhealthy
		result.Reason = reason
		return result, nil
	}

	result.Outcome = Healthy
	return result, nil
}

func Evaluate(snap Snapshot) (healthy bool, reason string) {
	for _, c := range snap.Containers {
		if c.State != StateRunning && !c.Completed() {
			return false, fmt.Sprintf("not running: %s (state=%s)", c.Service, c.State)
		}
		if c.Health != "" && c.Health != HealthNone && c.Health != HealthHealthy {
			return false, fmt.Sprintf("not healthy: %s (health=%s)", c.Service, c.Health)
		}
	}
	return true, ""
}
