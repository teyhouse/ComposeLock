package health

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"
)

const (
	HealthNone      = "none"
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

const (
	StateRunning    = "running"
	StateExited     = "exited"
	StateCreated    = "created"
	StatePaused     = "paused"
	StateRestarting = "restarting"
	StateDead       = "dead"
	StateRemoving   = "removing"
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
	Commit               string
	WatchDuration        time.Duration
	PollInterval         time.Duration
	UnhealthyStreakLimit int
	RestartTolerance     int
	ExpectContainers     bool
}

type Result struct {
	Outcome  Outcome
	Reason   string
	Failures []string
	Baseline Snapshot
}

func Watch(ctx context.Context, snap Snapshotter, clock Clock, opts Options, log *slog.Logger) (Result, error) {
	log = log.With("project", opts.ProjectName)
	if opts.WatchDuration <= 0 {
		log.Info("health watch: disabled, skipping verification", "commit", opts.Commit)
		return Result{Outcome: Healthy}, nil
	}

	baseline, err := snap.Snapshot(ctx, opts.ProjectName)
	if err != nil {
		return Result{}, fmt.Errorf("baseline snapshot: %w", err)
	}
	if res, ok := checkPopulated(baseline, opts); !ok {
		log.Warn("health watch: no containers for project", "commit", opts.Commit)
		res.Baseline = baseline
		return res, nil
	}
	baselineRestarts := make(map[string]int, len(baseline.Containers))
	baselineService := make(map[string]string, len(baseline.Containers))
	baselineCounts := make(map[string]int, len(baseline.Containers))
	for _, c := range baseline.Containers {
		baselineRestarts[c.ID] = c.RestartCount
		baselineService[c.ID] = c.Service
		baselineCounts[c.Service]++
	}

	deadline := clock.Now().Add(opts.WatchDuration)
	log.Info("health watch: starting",
		"commit", opts.Commit,
		"duration", opts.WatchDuration.String(),
		"poll_interval", opts.PollInterval.String(),
		"healthy_at", deadline.Format(time.RFC3339))
	result := Result{Baseline: baseline}
	unhealthyStreaks := make(map[string]int, len(baseline.Containers))
	wedgedStreaks := make(map[string]int, len(baseline.Containers))
	last := baseline

	for first := true; first || clock.Now().Before(deadline); first = false {
		if !clock.Sleep(ctx, opts.PollInterval) {
			return Result{}, ctx.Err()
		}

		cur, err := snap.Snapshot(ctx, opts.ProjectName)
		if err != nil {
			return Result{}, fmt.Errorf("health snapshot: %w", err)
		}
		last = cur

		if res, ok := checkPopulated(cur, opts); !ok {
			log.Warn("health watch: all containers disappeared")
			res.Baseline = baseline
			return res, nil
		}
		if reason, ok := missingContainers(baselineService, baselineCounts, cur); !ok {
			result.Reason = fmt.Sprintf("container disappeared: %s", reason)
			result.Failures = append(result.Failures, result.Reason)
			log.Warn("health watch: container disappeared", "detail", reason)
			result.Outcome = Unhealthy
			return result, nil
		}

		for _, c := range cur.Containers {
			if c.State == StateExited && !c.Completed() {
				result.Reason = fmt.Sprintf("container exited: %s (exit code %d)", c.Service, c.ExitCode)
				result.Failures = append(result.Failures, result.Reason)
				log.Warn("health watch: container exited", "service", c.Service, "exit_code", c.ExitCode)
				result.Outcome = Unhealthy
				return result, nil
			}
			if c.State == StateDead || c.State == StateRemoving {
				result.Reason = fmt.Sprintf("container %s: %s", c.State, c.Service)
				result.Failures = append(result.Failures, result.Reason)
				log.Warn("health watch: container in terminal state", "service", c.Service, "state", c.State)
				result.Outcome = Unhealthy
				return result, nil
			}
			if restarts := restartDelta(baselineRestarts, c); restarts > opts.RestartTolerance {
				result.Reason = fmt.Sprintf("restarted beyond tolerance: %s (restarts=%d, tolerance=%d)", c.Service, restarts, opts.RestartTolerance)
				result.Failures = append(result.Failures, result.Reason)
				log.Warn("health watch: restart tolerance exceeded", "service", c.Service, "restarts", restarts)
				result.Outcome = Unhealthy
				return result, nil
			}
			if c.Completed() {
				continue
			}
			if c.State != StateRunning {
				wedgedStreaks[c.ID]++
				if wedgedStreaks[c.ID] >= opts.UnhealthyStreakLimit {
					result.Reason = "container never started"
					result.Failures = append(result.Failures, result.Reason)
					log.Warn("health watch: container stuck in a non-running state",
						"service", c.Service, "state", c.State, "streak", wedgedStreaks[c.ID])
					result.Outcome = Unhealthy
					return result, nil
				}
				continue
			}
			delete(wedgedStreaks, c.ID)

			if c.Health == "" || c.Health == HealthNone || c.Health == HealthHealthy {
				delete(unhealthyStreaks, c.ID)
				continue
			}
			if c.Health == HealthStarting {
				continue
			}
			unhealthyStreaks[c.ID]++
			if unhealthyStreaks[c.ID] >= opts.UnhealthyStreakLimit {
				result.Reason = "healthcheck unhealthy"
				result.Failures = append(result.Failures, result.Reason)
				log.Warn("health watch: unhealthy streak limit reached",
					"service", c.Service, "health", c.Health, "streak", unhealthyStreaks[c.ID])
				result.Outcome = Unhealthy
				return result, nil
			}
		}
	}

	// A container stuck in "starting" for the whole window never triggers
	// the loop's per-poll checks above, so it must fail here explicitly.
	healthy, reason := evaluate(last, false, true)
	if !healthy {
		result.Outcome = Unhealthy
		result.Reason = reason
		return result, nil
	}

	result.Outcome = Healthy
	return result, nil
}

func restartDelta(baselineRestarts map[string]int, c ContainerStatus) int {
	base, known := baselineRestarts[c.ID]
	if !known {
		return c.RestartCount
	}
	return c.RestartCount - base
}

func checkPopulated(snap Snapshot, opts Options) (Result, bool) {
	if !opts.ExpectContainers || len(snap.Containers) > 0 {
		return Result{}, true
	}
	reason := "no containers running for project " + opts.ProjectName
	return Result{Outcome: Unhealthy, Reason: reason, Failures: []string{reason}}, false
}

func ChangedServices(before, after Snapshot) []string {
	beforeIDs, afterIDs := containerIDs(before), containerIDs(after)
	var changed []string
	for service, ids := range afterIDs {
		if !slices.Equal(ids, beforeIDs[service]) {
			changed = append(changed, service)
		}
	}
	for service := range beforeIDs {
		if _, ok := afterIDs[service]; !ok {
			changed = append(changed, service)
		}
	}
	slices.Sort(changed)
	return changed
}

func containerIDs(snap Snapshot) map[string][]string {
	byService := make(map[string][]string, len(snap.Containers))
	for _, c := range snap.Containers {
		byService[c.Service] = append(byService[c.Service], c.ID)
	}
	for _, ids := range byService {
		slices.Sort(ids)
	}
	return byService
}

func missingContainers(baselineService map[string]string, baselineCounts map[string]int, cur Snapshot) (string, bool) {
	present := make(map[string]struct{}, len(cur.Containers))
	counts := make(map[string]int, len(cur.Containers))
	for _, c := range cur.Containers {
		present[c.ID] = struct{}{}
		counts[c.Service]++
	}
	for _, id := range slices.Sorted(maps.Keys(baselineService)) {
		if _, ok := present[id]; ok {
			continue
		}
		service := baselineService[id]
		if got, want := counts[service], baselineCounts[service]; got < want {
			return fmt.Sprintf("%s (%d of %d replicas)", service, got, want), false
		}
		return fmt.Sprintf("%s (container %s was replaced)", service, shortID(id)), false
	}
	return "", true
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func RestartedServices(before, after Snapshot) []string {
	restarts := make(map[string]int, len(before.Containers))
	for _, c := range before.Containers {
		restarts[c.ID] = c.RestartCount
	}
	var restarted []string
	for _, c := range after.Containers {
		if restartDelta(restarts, c) > 0 {
			restarted = append(restarted, c.Service)
		}
	}
	slices.Sort(restarted)
	return slices.Compact(restarted)
}

func Evaluate(snap Snapshot) (healthy bool, reason string) {
	return evaluate(snap, false, false)
}

func EvaluatePreflight(snap Snapshot) (healthy bool, reason string) {
	return evaluate(snap, true, true)
}

func evaluate(snap Snapshot, tolerateStarting, tolerateRestarting bool) (healthy bool, reason string) {
	for _, c := range snap.Containers {
		if c.Completed() {
			continue
		}
		if tolerateRestarting && c.State == StateRestarting {
			continue
		}
		if c.State != StateRunning {
			return false, fmt.Sprintf("not running: %s (state=%s)", c.Service, c.State)
		}
		if tolerateStarting && c.Health == HealthStarting {
			continue
		}
		if c.Health != "" && c.Health != HealthNone && c.Health != HealthHealthy {
			return false, fmt.Sprintf("not healthy: %s (health=%s)", c.Service, c.Health)
		}
	}
	return true, ""
}
