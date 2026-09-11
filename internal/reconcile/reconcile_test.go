package reconcile

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

// ---- fakes ----

type fakeRunner struct {
	responses map[string]string
	errors    map[string]error
}

func (f *fakeRunner) Run(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, []byte, error) {
	key := name + " " + strings.Join(args, " ")
	if err, ok := f.errors[key]; ok {
		return nil, nil, err
	}
	return []byte(f.responses[key]), nil, nil
}

type fakeCompose struct {
	loadErr error
	upErr   error
	project *ctypes.Project
	upCalls int
}

func (f *fakeCompose) LoadProject(_ context.Context, _, projectName string) (*ctypes.Project, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if f.project != nil {
		return f.project, nil
	}
	return &ctypes.Project{Name: projectName, Services: ctypes.Services{"web": ctypes.ServiceConfig{Name: "web"}}}, nil
}

func (f *fakeCompose) Up(_ context.Context, _ *ctypes.Project) error {
	f.upCalls++
	return f.upErr
}

func (f *fakeCompose) Down(context.Context, string) error { return nil }

// fakeSnapshotter returns snapshots[i] on the i-th call, clamped to the
// last entry once exhausted.
type fakeSnapshotter struct {
	snapshots []health.Snapshot
	call      int
}

func (f *fakeSnapshotter) Snapshot(context.Context, string) (health.Snapshot, error) {
	i := min(f.call, len(f.snapshots)-1)
	f.call++
	return f.snapshots[i], nil
}

func healthySnapshot() health.Snapshot {
	return health.Snapshot{Containers: []health.ContainerStatus{
		{ID: "c1", Service: "web", State: health.StateRunning},
	}}
}

func unhealthySnapshot() health.Snapshot {
	return health.Snapshot{Containers: []health.ContainerStatus{
		{ID: "c1", Service: "web", State: health.StateExited},
	}}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) bool {
	c.now = c.now.Add(d)
	return true
}

func testDeps(t *testing.T, runner *fakeRunner, compose *fakeCompose, snap health.Snapshotter, st *state.State) Deps {
	t.Helper()
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
	return Deps{
		Config:   cfg,
		Git:      &git.Syncer{Runner: runner, RepoPath: cfg.RepoPath, Remote: cfg.Remote, Branch: cfg.Branch},
		Compose:  compose,
		Health:   snap,
		Clock:    &fakeClock{},
		State:    &state.MemStore{State: st},
		Notifier: notify.New("", slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))),
		Log:      slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
}

func gitNoChange(commit string) *fakeRunner {
	return &fakeRunner{responses: map[string]string{
		"git fetch origin main":     "",
		"git rev-parse HEAD":        commit,
		"git rev-parse origin/main": commit,
	}}
}

func gitChange(old, new string) *fakeRunner {
	return &fakeRunner{responses: map[string]string{
		"git fetch origin main":                   "",
		"git rev-parse HEAD":                      old,
		"git rev-parse origin/main":               new,
		"git diff --name-only " + old + " " + new: "docker-compose.yml",
		"git checkout " + new:                     "",
		"git checkout " + old:                     "",
	}}
}

// ---- tests ----

func TestReconcileNoChange(t *testing.T) {
	st := state.New()
	compose := &fakeCompose{}
	deps := testDeps(t, gitNoChange("abc123"), compose, &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot()}}, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Changed {
		t.Error("expected Changed = false")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
}

func TestReconcileApplySucceedsPromotesState(t *testing.T) {
	st := state.New()
	compose := &fakeCompose{}
	snap := &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot()}}
	deps := testDeps(t, gitChange("old111", "new222"), compose, snap, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected Applied = true, result = %+v", result)
	}
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1", compose.upCalls)
	}
	if st.LastHealthyCommit != "new222" {
		t.Errorf("LastHealthyCommit = %q, want %q", st.LastHealthyCommit, "new222")
	}
	if st.Pending() {
		t.Error("expected pending_commit cleared after promotion")
	}
	if st.LastResult != state.ResultSuccess {
		t.Errorf("LastResult = %q, want %q", st.LastResult, state.ResultSuccess)
	}
}

func TestReconcileKnownBadCommitSkipped(t *testing.T) {
	st := state.New()
	st.LastFailedCommit = "new222"
	compose := &fakeCompose{}
	deps := testDeps(t, gitChange("old111", "new222"), compose, &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot()}}, st)

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if !result.Skipped {
		t.Fatalf("expected Skipped = true, result = %+v", result)
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0 (known-bad commit)", compose.upCalls)
	}
}

func TestReconcileKnownBadCommitForced(t *testing.T) {
	st := state.New()
	st.LastFailedCommit = "new222"
	compose := &fakeCompose{}
	deps := testDeps(t, gitChange("old111", "new222"), compose, &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot()}}, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli", Force: true}, deps)

	if result.Skipped {
		t.Fatal("expected Skipped = false when --force is set")
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (forced past known-bad guard)", compose.upCalls)
	}
}

func TestReconcilePreflightUnhealthyRevertsWithoutApplying(t *testing.T) {
	st := state.New()
	st.LastHealthyCommit = "old111"
	compose := &fakeCompose{}
	// call0: pre-flight (unhealthy) -> triggers revert.
	// call1: revert's watch baseline+poll -> healthy.
	snap := &fakeSnapshotter{snapshots: []health.Snapshot{unhealthySnapshot(), healthySnapshot()}}
	deps := testDeps(t, gitChange("old111", "new222"), compose, snap, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (only the revert apply, not the new commit)", compose.upCalls)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if result.RolledBackTo != "old111" {
		t.Errorf("RolledBackTo = %q, want %q", result.RolledBackTo, "old111")
	}
}

func TestReconcileMissingEnvFileAbortsBeforeUp(t *testing.T) {
	st := state.New()
	compose := &fakeCompose{
		project: &ctypes.Project{
			Name:       "test-stack",
			WorkingDir: t.TempDir(),
			Services: ctypes.Services{
				"web": ctypes.ServiceConfig{Name: "web", EnvFiles: []ctypes.EnvFile{{Path: "missing.env"}}},
			},
		},
	}
	deps := testDeps(t, gitChange("old111", "new222"), compose, &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot()}}, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err == nil {
		t.Fatal("expected error for missing env_file")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
	if st.Pending() {
		t.Error("expected state unchanged (no pending_commit) on env_file failure")
	}
}

func TestReconcileApplyFailsNoBaselineDegrades(t *testing.T) {
	st := state.New() // no LastHealthyCommit: first run, no baseline
	compose := &fakeCompose{upErr: errors.New("image pull failed")}
	deps := testDeps(t, gitChange("old111", "new222"), compose, &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot()}}, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err == nil {
		t.Fatal("expected an error")
	}
	if st.LastResult != state.ResultFailedApply {
		t.Errorf("LastResult = %q, want %q (Up itself failed, degrade happens on next crash-recovery pass)", st.LastResult, state.ResultFailedApply)
	}
	if !st.Pending() {
		t.Error("expected pending_commit to remain set for crash recovery")
	}
}

func TestReconcileUnhealthyWatchRevertsSuccessfully(t *testing.T) {
	st := state.New()
	st.LastHealthyCommit = "old111"
	compose := &fakeCompose{}
	// call0: pre-flight (healthy). call1: apply-watch baseline (healthy).
	// call2: apply-watch poll (exited -> fails). call3: revert-watch
	// baseline (healthy). call4: revert-watch poll (healthy -> passes).
	snap := &fakeSnapshotter{snapshots: []health.Snapshot{
		healthySnapshot(), healthySnapshot(), unhealthySnapshot(), healthySnapshot(), healthySnapshot(),
	}}
	deps := testDeps(t, gitChange("old111", "new222"), compose, snap, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if st.LastHealthyCommit != "old111" {
		t.Errorf("LastHealthyCommit = %q, want unchanged %q", st.LastHealthyCommit, "old111")
	}
	if st.LastFailedCommit != "new222" {
		t.Errorf("LastFailedCommit = %q, want %q (guard should keep skipping it)", st.LastFailedCommit, "new222")
	}
	if st.LastResult != state.ResultReverted {
		t.Errorf("LastResult = %q, want %q", st.LastResult, state.ResultReverted)
	}
	if compose.upCalls != 2 {
		t.Errorf("Up called %d times, want 2 (initial apply + revert apply)", compose.upCalls)
	}
}

func TestReconcileRevertAlsoFailsDegrades(t *testing.T) {
	st := state.New()
	st.LastHealthyCommit = "old111"
	compose := &fakeCompose{}
	// pre-flight: healthy. apply watch: exited. revert watch: exited too.
	snap := &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot(), unhealthySnapshot(), unhealthySnapshot()}}
	deps := testDeps(t, gitChange("old111", "new222"), compose, snap, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Degraded {
		t.Fatalf("expected Degraded = true, result = %+v", result)
	}
	if st.LastResult != state.ResultDegraded {
		t.Errorf("LastResult = %q, want %q", st.LastResult, state.ResultDegraded)
	}
	if !st.Pending() {
		t.Error("expected pending_commit to stay set for visibility")
	}

	// Subsequent run without --force must refuse to touch anything further.
	compose.upCalls = 0
	deps2 := testDeps(t, gitNoChange("new222"), compose, snap, st)
	result2 := Reconcile(t.Context(), Options{Trigger: "poll"}, deps2)
	if !result2.Degraded {
		t.Fatalf("expected still Degraded without --force, result = %+v", result2)
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times on degraded retry without --force, want 0", compose.upCalls)
	}
}

func TestReconcileCrashRecoveryHealthyPromotesFreshWatch(t *testing.T) {
	st := state.New()
	st.PendingCommit = "pending333"
	st.LastHealthyCommit = "old111"
	compose := &fakeCompose{}
	snap := &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot()}}
	deps := testDeps(t, gitNoChange("pending333"), compose, snap, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if st.LastHealthyCommit != "pending333" {
		t.Errorf("LastHealthyCommit = %q, want %q", st.LastHealthyCommit, "pending333")
	}
	if st.Pending() {
		t.Error("expected pending_commit cleared")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0 (crash recovery only re-watches, doesn't re-apply)", compose.upCalls)
	}
}

func TestReconcileCrashRecoveryUnhealthyRevertsImmediately(t *testing.T) {
	st := state.New()
	st.PendingCommit = "pending333"
	st.LastHealthyCommit = "old111"
	compose := &fakeCompose{}
	// live snapshot: unhealthy -> immediate revert; revert watch: healthy.
	snap := &fakeSnapshotter{snapshots: []health.Snapshot{unhealthySnapshot(), healthySnapshot()}}
	deps := testDeps(t, gitNoChange("pending333"), compose, snap, st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (the revert apply)", compose.upCalls)
	}
}

func TestReconcileDryRunNoSDKCallsNoStateWrites(t *testing.T) {
	st := state.New()
	compose := &fakeCompose{}
	deps := testDeps(t, gitChange("old111", "new222"), compose, &fakeSnapshotter{snapshots: []health.Snapshot{healthySnapshot()}}, st)

	result := Reconcile(t.Context(), Options{DryRun: true, Trigger: "cli"}, deps)

	if !result.Changed {
		t.Error("expected dry run to still report Changed = true")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0 on dry run", compose.upCalls)
	}
	if st.PendingCommit != "" || st.LastHealthyCommit != "" {
		t.Errorf("expected no state writes on dry run, got %+v", st)
	}
}

func TestReconcileSingleFlightSkipsConcurrentRun(t *testing.T) {
	st := state.New()
	compose := &fakeCompose{}
	block := make(chan struct{})
	release := make(chan struct{})

	blockingSnap := &blockingSnapshotter{healthy: healthySnapshot(), block: block, release: release}
	deps := testDeps(t, gitChange("old111", "new222"), compose, blockingSnap, st)

	done := make(chan Result, 1)
	go func() {
		done <- Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	}()

	<-block // wait until the first Reconcile is inside the pre-flight snapshot, holding the lock

	second := Reconcile(t.Context(), Options{Trigger: "webhook"}, deps)
	if !second.Skipped {
		t.Fatalf("expected concurrent Reconcile to be Skipped, got %+v", second)
	}

	close(release)
	first := <-done
	if first.Skipped {
		t.Fatal("first Reconcile should not report Skipped")
	}
}

type blockingSnapshotter struct {
	healthy health.Snapshot
	block   chan struct{}
	release chan struct{}
	once    bool
}

func (b *blockingSnapshotter) Snapshot(context.Context, string) (health.Snapshot, error) {
	if !b.once {
		b.once = true
		close(b.block)
		<-b.release
	}
	return b.healthy, nil
}
