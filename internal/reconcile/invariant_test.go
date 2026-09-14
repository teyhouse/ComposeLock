package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

const (
	defaultPropertySeed = 1
	defaultPropertyRuns = 1000
	quietRuns           = 4
	maxEvents           = 10
)

type event int

const (
	evQuiet event = iota
	evPushCommit
	evPushDocsCommit
	evGateUnhealthy
	evWatchUnhealthy
	evUpFails
	evLoadFails
	evFetchFails
	evDaemonDown
	evCancelUp
	evForce
	eventCount
)

func (e event) String() string {
	switch e {
	case evQuiet:
		return "quiet"
	case evPushCommit:
		return "pushCommit"
	case evPushDocsCommit:
		return "pushDocsCommit"
	case evGateUnhealthy:
		return "gateUnhealthy"
	case evWatchUnhealthy:
		return "watchUnhealthy"
	case evUpFails:
		return "upFails"
	case evLoadFails:
		return "loadFails"
	case evFetchFails:
		return "fetchFails"
	case evDaemonDown:
		return "daemonDown"
	case evCancelUp:
		return "cancelUp"
	case evForce:
		return "force"
	}
	return "?"
}

type invCompose struct {
	mu      sync.Mutex
	loadErr error
	upErr   error
	onUp    func()
}

func (c *invCompose) LoadProject(_ context.Context, _ []string, projectName string) (*ctypes.Project, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loadErr != nil {
		return nil, c.loadErr
	}
	return &ctypes.Project{Name: projectName, Services: ctypes.Services{"web": ctypes.ServiceConfig{Name: "web"}}}, nil
}

func (c *invCompose) Up(ctx context.Context, _ *ctypes.Project) error {
	c.mu.Lock()
	onUp, upErr := c.onUp, c.upErr
	c.mu.Unlock()
	if onUp != nil {
		onUp()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	return upErr
}

func (c *invCompose) Down(context.Context, string) error { return nil }

type invSnapshotter struct {
	mu            sync.Mutex
	err           error
	healthyBefore int
	calls         int
}

func (s *invSnapshotter) Snapshot(context.Context, string) (health.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return health.Snapshot{}, s.err
	}
	s.calls++
	if s.healthyBefore < 0 || s.calls <= s.healthyBefore {
		return healthySnapshot(), nil
	}
	return unhealthySnapshot(), nil
}

func (s *invSnapshotter) setHealthy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err, s.healthyBefore, s.calls = nil, -1, 0
}

func (s *invSnapshotter) setUnhealthyAfter(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err, s.healthyBefore, s.calls = nil, n, 0
}

func (s *invSnapshotter) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// commitID keeps generated commits valid: git.validCommit requires 4 to 64 hex
// characters, and the leading c is itself a hex digit.
func commitID(n int) string { return fmt.Sprintf("c%05d", n) }

type world struct {
	git     *fakeGit
	compose *invCompose
	snap    *invSnapshotter
	store   *state.MemStore
	deps    Deps
	commits int
	// branchAt is the branch head as the run under inspection saw it.
	branchAt string
	// sawBranch is false when the run could not fetch, so it cannot know the head.
	sawBranch bool
	// branchBad records whether the head was known-bad when the run started: a
	// successful promote clears the failed list, so this cannot be read afterwards.
	branchBad bool
	// promoted records every commit a run actually applied and watched healthy.
	promoted map[string]bool
}

func newWorld() *world {
	g := &fakeGit{head: commitID(0), remote: commitID(0)}
	compose := &invCompose{}
	snap := &invSnapshotter{healthyBefore: -1}
	store := &state.MemStore{State: state.New()}
	cfg := &config.Config{
		RepoPath:                  "/repo",
		Remote:                    "origin",
		Branch:                    "main",
		ComposeFile:               "docker-compose.yml",
		ProjectName:               "test-stack",
		RetryAttempts:             1,
		RetryDelaySeconds:         0,
		HealthWatchSeconds:        5,
		HealthPollIntervalSeconds: 5,
		HealthUnhealthyStreak:     3,
		HealthRestartTolerance:    1,
	}
	return &world{
		git:      g,
		compose:  compose,
		snap:     snap,
		store:    store,
		promoted: map[string]bool{},
		deps: Deps{
			Config:   cfg,
			Git:      &git.Syncer{Runner: g, RepoPath: cfg.RepoPath, Remote: cfg.Remote, Branch: cfg.Branch},
			Compose:  compose,
			Health:   snap,
			Clock:    &fakeClock{},
			State:    store,
			Notifier: notify.New("", slog.New(slog.DiscardHandler)),
			Log:      slog.New(slog.DiscardHandler),
		},
	}
}

func (w *world) push(diff string) {
	w.commits++
	w.git.remote = commitID(w.commits)
	w.git.diffFiles = diff
}

// step applies one event to the world, runs a single Reconcile, and returns it.
func (w *world) step(e event) Result {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w.snap.setHealthy()
	w.compose.mu.Lock()
	w.compose.loadErr, w.compose.upErr, w.compose.onUp = nil, nil, nil
	w.compose.mu.Unlock()
	w.git.fetchErr = nil
	opts := Options{Trigger: "poll"}

	// Every adverse event also lands a commit. Once a deployment has converged
	// there is nothing to do, so an adverse condition on its own would make the
	// run a no-op and the rest of the sequence inert.
	switch e {
	case evPushCommit:
		w.push("docker-compose.yml")
	case evPushDocsCommit:
		w.push("README.md")
	case evGateUnhealthy:
		w.push("docker-compose.yml")
		w.snap.setUnhealthyAfter(0)
	case evWatchUnhealthy:
		w.push("docker-compose.yml")
		w.snap.setUnhealthyAfter(1)
	case evUpFails:
		w.push("docker-compose.yml")
		w.compose.upErr = errors.New("image pull failed")
	case evLoadFails:
		w.push("docker-compose.yml")
		w.compose.loadErr = errors.New("yaml: mapping values are not allowed")
	case evFetchFails:
		w.git.fetchErr = errors.New("could not resolve host")
	case evDaemonDown:
		w.push("docker-compose.yml")
		w.snap.setErr(errors.New("docker daemon unreachable"))
	case evCancelUp:
		w.push("docker-compose.yml")
		w.compose.onUp = cancel
	case evForce:
		opts.Force = true
	}

	before := w.store.State.LastHealthyCommit
	w.branchAt = w.git.remote
	w.sawBranch = w.git.fetchErr == nil
	w.branchBad = w.store.State.IsKnownBad(w.git.remote)
	res := Reconcile(ctx, opts, w.deps)
	if after := w.store.State.LastHealthyCommit; after != "" && after != before {
		w.promoted[after] = true
	}
	return res
}

// check returns a description of the first invariant this state violates.
func (w *world) check(e event, res Result) string {
	st := w.store.State

	if st.LastHealthyCommit != "" && !w.promoted[st.LastHealthyCommit] {
		return fmt.Sprintf("last_healthy_commit %q was never applied and watched healthy", st.LastHealthyCommit)
	}
	if st.LastResult == state.ResultDegraded && st.Pending() {
		return fmt.Sprintf("DEGRADED while still pending (pending_commit %q, pending_revert %v): --force would resume instead of re-deciding",
			st.PendingCommit, st.PendingRevert)
	}
	if st.LastHealthyCommit != "" && st.IsKnownBad(st.LastHealthyCommit) {
		return fmt.Sprintf("commit %q is both the rollback target and known-bad", st.LastHealthyCommit)
	}
	if st.IsKnownBad(w.branchAt) && st.LastResult == state.ResultSuccess && st.LastHealthyCommit != w.branchAt {
		return fmt.Sprintf("run over known-bad commit %q reported success without deploying it: a known-bad commit must be recorded as skipped, not silently accepted",
			w.branchAt)
	}
	// Checked on the revert path: a promoted deploy clears the counter anyway, so
	// only a run that got past the gate and then failed can still show a stale one.
	if res.Applied && res.Reverted && st.PreflightBlocks != 0 {
		return fmt.Sprintf("preflight_blocks is %d after applying %q: the gate passed this run, so the consecutive-block streak should have been cleared",
			st.PreflightBlocks, res.NewCommit)
	}
	if res.Applied && w.sawBranch && !w.branchBad && res.NewCommit != w.branchAt {
		return fmt.Sprintf("applied %q while the branch was at %q: deploying anything but the branch head wastes a watch window on a commit already superseded (the fetch succeeded and that head is not known-bad, so it was both visible and eligible)",
			res.NewCommit, w.branchAt)
	}
	if res.Err == nil && st.LastCheckoutCommit != "" && st.ExpectedCheckout() != w.git.head {
		return fmt.Sprintf("expected checkout %q but HEAD is %q after a run that reported no error",
			st.ExpectedCheckout(), w.git.head)
	}
	return ""
}

// settle runs quiet reconciles and reports whether the deployment converged on
// the branch head, gave up explicitly, or is correctly refusing a known-bad commit.
func (w *world) settle() string {
	for range quietRuns {
		w.step(evQuiet)
		if v := w.check(evQuiet, Result{}); v != "" {
			return v
		}
	}
	st := w.store.State
	switch {
	// Converged means the worktree tracks the branch. last_healthy_commit is
	// deliberately not the test: a commit that changes nothing relevant advances
	// the checkout without ever becoming the rollback target.
	case w.git.head == w.git.remote && !st.Pending():
		return ""
	case st.LastResult == state.ResultDegraded:
		return ""
	case st.IsKnownBad(w.git.remote):
		return ""
	}
	return fmt.Sprintf("stuck: branch is at %q, HEAD is %q, last_healthy_commit %q, last_result %q, pending %q: %d quiet runs neither converged, gave up, nor refused a known-bad commit",
		w.git.remote, w.git.head, st.LastHealthyCommit, st.LastResult, st.PendingCommit, quietRuns)
}

func runSequence(events []event) string {
	w := newWorld()
	for _, e := range events {
		res := w.step(e)
		if v := w.check(e, res); v != "" {
			return v
		}
	}
	return w.settle()
}

func formatEvents(events []event) string {
	parts := make([]string, len(events))
	for i, e := range events {
		parts[i] = e.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// shrink greedily drops events while the sequence still fails, so a failure is
// reported as the shortest sequence that still reproduces it.
func shrink(events []event) []event {
	best := events
	for changed := true; changed; {
		changed = false
		for i := range best {
			candidate := make([]event, 0, len(best)-1)
			candidate = append(candidate, best[:i]...)
			candidate = append(candidate, best[i+1:]...)
			if runSequence(candidate) != "" {
				best = candidate
				changed = true
				break
			}
		}
	}
	return best
}

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

func TestReconcileStateInvariants(t *testing.T) {
	seed, runs := propertyConfig(t)
	rng := rand.New(rand.NewPCG(seed, 0x9E3779B97F4A7C15))

	for range runs {
		// Every sequence starts from a deployed stack: a fresh world has no
		// rollback target, so the first failure degrades and the run stops
		// exploring before it reaches the interesting states.
		n := 1 + rng.IntN(maxEvents)
		events := make([]event, 0, n+1)
		events = append(events, evQuiet)
		for range n {
			events = append(events, event(rng.IntN(int(eventCount))))
		}
		if violation := runSequence(events); violation != "" {
			minimal := shrink(events)
			t.Fatalf("invariant violated: %s\n  minimal sequence: %s\n original sequence: %s\n reproduce with: COMPOSELOCK_PROPERTY_SEED=%d",
				runSequence(minimal), formatEvents(minimal), formatEvents(events), seed)
		}
	}
}
