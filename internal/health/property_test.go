package health

import (
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"
)

const (
	defaultPropertySeed = 1
	defaultPropertyRuns = 2000
)

func propertyConfig(t *testing.T) (uint64, int) {
	t.Helper()
	seed, runs := uint64(defaultPropertySeed), defaultPropertyRuns
	if v := os.Getenv("COMPOSELOCK_PROPERTY_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("COMPOSELOCK_PROPERTY_SEED=%q: %v", v, err)
		}
		seed = n
	}
	if v := os.Getenv("COMPOSELOCK_PROPERTY_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("COMPOSELOCK_PROPERTY_RUNS=%q: %v", v, err)
		}
		runs = n
	}
	return seed, runs
}

// watchOpts pins the poll count exactly: the loop runs WatchDuration/PollInterval
// times, so a sequence of 1+polls snapshots is consumed one per call with no
// clamping, and every generated snapshot is one the watch actually saw.
func watchOpts(polls, streak, tolerance int) Options {
	const interval = 5 * time.Second
	return Options{
		ProjectName:          "prop",
		WatchDuration:        time.Duration(polls) * interval,
		PollInterval:         interval,
		UnhealthyStreakLimit: streak,
		RestartTolerance:     tolerance,
	}
}

func runWatch(t *testing.T, seq []Snapshot, opts Options) Result {
	t.Helper()
	res, err := Watch(t.Context(), &fakeSnapshotter{snapshots: seq}, &fakeClock{}, opts, testLog())
	if err != nil {
		t.Fatalf("Watch returned an error: %v", err)
	}
	return res
}

func clearlyFine(c ContainerStatus) bool {
	if c.State != StateRunning {
		return false
	}
	return c.Health == "" || c.Health == HealthNone || c.Health == HealthHealthy
}

// hardFailure reports a reason the watch must not return Healthy, using only
// checks with no streak logic in them, so it cannot drift from Watch's own
// bookkeeping the way a full reference implementation would.
func hardFailure(seq []Snapshot, tolerance int) string {
	baseline := seq[0]
	base := make(map[string]int, len(baseline.Containers))
	present := make(map[string]bool, len(baseline.Containers))
	for _, c := range baseline.Containers {
		base[c.ID] = c.RestartCount
		present[c.ID] = true
	}

	for _, snap := range seq[1:] {
		seen := make(map[string]bool, len(snap.Containers))
		for _, c := range snap.Containers {
			seen[c.ID] = true
			if c.State == StateExited && c.ExitCode != 0 {
				return "container exited non-zero"
			}
			if c.State == StateDead || c.State == StateRemoving {
				return "container in a terminal state"
			}
			delta := c.RestartCount
			if b, ok := base[c.ID]; ok {
				delta -= b
			}
			if delta > tolerance {
				return "restarts beyond tolerance"
			}
		}
		for id := range present {
			if !seen[id] {
				return "a baseline container disappeared"
			}
		}
	}
	if healthy, reason := evaluate(seq[len(seq)-1], false, true); !healthy {
		return "final snapshot: " + reason
	}
	return ""
}

// justified reports whether anything in the sequence could explain an Unhealthy
// verdict. Deliberately permissive: it answers "was there a reason at all", not
// "was it this exact reason".
func justified(seq []Snapshot, streak, tolerance int) bool {
	if hardFailure(seq, tolerance) != "" {
		return true
	}
	runs := map[string]int{}
	for _, snap := range seq[1:] {
		if len(snap.Containers) == 0 {
			return true
		}
		for _, c := range snap.Containers {
			switch {
			case c.Completed():
				// Held, not reset: the watch skips a completed container, so the
				// polls either side of it stay consecutive.
			case !clearlyFine(c):
				// A container reporting "starting" holds its run rather than
				// extending or breaking it, matching how the watch treats a
				// probe that has not reached a verdict yet.
				if c.Health != HealthStarting || c.State != StateRunning {
					runs[c.ID]++
				}
			default:
				runs[c.ID] = 0
			}
			if runs[c.ID] >= streak {
				return true
			}
		}
	}
	return false
}

func randomSnapshotSequence(rng *rand.Rand, containers, polls int) []Snapshot {
	ids := make([]string, containers)
	services := make([]string, containers)
	for i := range ids {
		ids[i] = "c" + strconv.Itoa(i)
		services[i] = "svc" + strconv.Itoa(i%2)
	}

	seq := make([]Snapshot, 0, polls+1)
	baseline := Snapshot{}
	restarts := make([]int, containers)
	for i := range ids {
		baseline.Containers = append(baseline.Containers, ContainerStatus{
			ID: ids[i], Service: services[i], State: StateRunning, Health: HealthHealthy,
		})
	}
	seq = append(seq, baseline)

	exited := make([]bool, containers)
	for range polls {
		snap := Snapshot{}
		for i := range ids {
			if rng.IntN(100) < 4 {
				continue // the container is missing from this poll
			}
			c := ContainerStatus{ID: ids[i], Service: services[i], State: StateRunning, RestartCount: restarts[i]}
			switch n := rng.IntN(100); {
			case n < 45:
				c.Health = HealthHealthy
			case n < 55:
				c.Health = HealthStarting
			case n < 68:
				c.Health = HealthUnhealthy
			case n < 72:
				c.Health = "degraded" // a value this version does not know
			case n < 80:
				c.State = StateRestarting
			case n < 85:
				c.State = StateCreated
			case n < 88:
				c.State = StatePaused
			case n < 92:
				c.State, c.ExitCode = StateExited, 0
			case n < 95:
				c.State, c.ExitCode = StateExited, 1
			case n < 97:
				c.State = StateDead
			default:
				restarts[i]++
				c.RestartCount = restarts[i]
				c.Health = HealthHealthy
			}
			// A container that has exited cannot be running again without having
			// restarted, so keep the restart count honest rather than generating
			// sequences Docker could never produce.
			if exited[i] && c.State != StateExited {
				restarts[i]++
				c.RestartCount = restarts[i]
			}
			exited[i] = c.State == StateExited
			snap.Containers = append(snap.Containers, c)
		}
		seq = append(seq, snap)
	}
	return seq
}

func TestWatchVerdictProperties(t *testing.T) {
	seed, runs := propertyConfig(t)
	rng := rand.New(rand.NewPCG(seed, 0x9E3779B97F4A7C15))

	for range runs {
		containers := 1 + rng.IntN(3)
		polls := 2 + rng.IntN(6)
		streak := 1 + rng.IntN(3)
		tolerance := rng.IntN(3)
		opts := watchOpts(polls, streak, tolerance)
		seq := randomSnapshotSequence(rng, containers, polls)

		res := runWatch(t, seq, opts)

		if res.Outcome == Healthy {
			if reason := hardFailure(seq, tolerance); reason != "" {
				t.Fatalf("watch passed a sequence it should have failed (%s)\nstreak=%d tolerance=%d\n%s",
					reason, streak, tolerance, formatSequence(seq))
			}
		}
		if res.Outcome == Unhealthy && !justified(seq, streak, tolerance) {
			t.Fatalf("watch failed a sequence with nothing wrong in it (reason %q)\nstreak=%d tolerance=%d\n%s",
				res.Reason, streak, tolerance, formatSequence(seq))
		}

		again := runWatch(t, seq, opts)
		if again.Outcome != res.Outcome || again.Reason != res.Reason {
			t.Fatalf("watch is not deterministic: %v/%q then %v/%q\n%s",
				res.Outcome, res.Reason, again.Outcome, again.Reason, formatSequence(seq))
		}
	}
}

// TestWatchStreakProperties pins the per-container streak semantics: a single
// container sick for the whole limit fails, the same container sick for one poll
// fewer does not, and two containers alternating never add up to a streak that
// neither of them had.
func TestWatchStreakProperties(t *testing.T) {
	seed, runs := propertyConfig(t)
	rng := rand.New(rand.NewPCG(seed, 0x2545F4914F6CDD1D))

	sick := []ContainerStatus{
		{ID: "c0", Service: "web", State: StateRunning, Health: HealthUnhealthy},
		{ID: "c0", Service: "web", State: StateRestarting},
		{ID: "c0", Service: "web", State: StateCreated},
		{ID: "c0", Service: "web", State: StateRunning, Health: "degraded"},
	}
	well := ContainerStatus{ID: "c0", Service: "web", State: StateRunning, Health: HealthHealthy}

	for range runs {
		streak := 2 + rng.IntN(3)
		bad := sick[rng.IntN(len(sick))]
		polls := streak + 2

		full := []Snapshot{{Containers: []ContainerStatus{well}}}
		for i := range polls {
			c := well
			if i < streak {
				c = bad
			}
			full = append(full, Snapshot{Containers: []ContainerStatus{c}})
		}
		if res := runWatch(t, full, watchOpts(polls, streak, 1)); res.Outcome != Unhealthy {
			t.Fatalf("a container %s/%q for %d consecutive polls must fail the watch, got %v\n%s",
				bad.State, bad.Health, streak, res.Outcome, formatSequence(full))
		}

		short := []Snapshot{{Containers: []ContainerStatus{well}}}
		for i := range polls {
			c := well
			if i < streak-1 {
				c = bad
			}
			short = append(short, Snapshot{Containers: []ContainerStatus{c}})
		}
		if res := runWatch(t, short, watchOpts(polls, streak, 1)); res.Outcome != Healthy {
			t.Fatalf("a container %s/%q for %d polls, one short of the limit, must not fail the watch, got %v (%s)\n%s",
				bad.State, bad.Health, streak-1, res.Outcome, res.Reason, formatSequence(short))
		}

		unhealthyA := ContainerStatus{ID: "c0", Service: "a", State: StateRunning, Health: HealthUnhealthy}
		healthyA := ContainerStatus{ID: "c0", Service: "a", State: StateRunning, Health: HealthHealthy}
		unhealthyB := ContainerStatus{ID: "c1", Service: "b", State: StateRunning, Health: HealthUnhealthy}
		healthyB := ContainerStatus{ID: "c1", Service: "b", State: StateRunning, Health: HealthHealthy}
		alternating := []Snapshot{{Containers: []ContainerStatus{healthyA, healthyB}}}
		for i := range 2 * streak {
			if i%2 == 0 {
				alternating = append(alternating, Snapshot{Containers: []ContainerStatus{unhealthyA, healthyB}})
				continue
			}
			alternating = append(alternating, Snapshot{Containers: []ContainerStatus{healthyA, unhealthyB}})
		}
		alternating = append(alternating, Snapshot{Containers: []ContainerStatus{healthyA, healthyB}})
		if res := runWatch(t, alternating, watchOpts(len(alternating)-1, streak, 1)); res.Outcome != Healthy {
			t.Fatalf("two services unhealthy on alternate polls must not add up to a streak neither of them had, got %v (%s)\n%s",
				res.Outcome, res.Reason, formatSequence(alternating))
		}
	}
}

func TestRestartedServicesProperties(t *testing.T) {
	seed, runs := propertyConfig(t)
	rng := rand.New(rand.NewPCG(seed, 0x14057B7EF767814F))

	for range runs {
		before := Snapshot{Containers: []ContainerStatus{
			{ID: "c0", Service: "web", RestartCount: rng.IntN(5)},
			{ID: "c1", Service: "db", RestartCount: rng.IntN(5)},
		}}
		bump := rng.IntN(3)
		after := Snapshot{Containers: []ContainerStatus{
			{ID: "c0", Service: "web", RestartCount: before.Containers[0].RestartCount + bump},
			{ID: "c1", Service: "db", RestartCount: before.Containers[1].RestartCount},
		}}

		got := RestartedServices(before, after)
		if bump > 0 && (len(got) != 1 || got[0] != "web") {
			t.Fatalf("RestartedServices = %v, want [web] after %d in-place restarts", got, bump)
		}
		if bump == 0 && len(got) != 0 {
			t.Fatalf("RestartedServices = %v, want empty when nothing restarted", got)
		}

		fresh := rng.IntN(4)
		replaced := Snapshot{Containers: []ContainerStatus{
			{ID: "c9", Service: "web", RestartCount: fresh},
			{ID: "c1", Service: "db", RestartCount: before.Containers[1].RestartCount},
		}}
		got = RestartedServices(before, replaced)
		if fresh > 0 && (len(got) != 1 || got[0] != "web") {
			t.Fatalf("RestartedServices = %v, want [web]: a replacement that restarted %d times since it was created did restart", got, fresh)
		}
		if fresh == 0 && len(got) != 0 {
			t.Fatalf("RestartedServices = %v, want empty: a replacement that never restarted is an update, not a restart", got)
		}
	}
}

func formatSequence(seq []Snapshot) string {
	out := ""
	for i, snap := range seq {
		label := "poll " + strconv.Itoa(i)
		if i == 0 {
			label = "baseline"
		}
		out += label + ":"
		if len(snap.Containers) == 0 {
			out += " (no containers)"
		}
		for _, c := range snap.Containers {
			out += " {" + c.ID + " " + c.Service + " " + c.State
			if c.Health != "" {
				out += "/" + c.Health
			}
			if c.RestartCount != 0 {
				out += " restarts=" + strconv.Itoa(c.RestartCount)
			}
			if c.State == StateExited {
				out += " exit=" + strconv.Itoa(c.ExitCode)
			}
			out += "}"
		}
		out += "\n"
	}
	return out
}
