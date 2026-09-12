package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

type fakeGit struct {
	head      string
	remote    string
	diffFiles string // git diff --name-only output; defaults to "docker-compose.yml" when empty
}

func (g *fakeGit) Run(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "fetch":
		return nil, nil, nil
	case "rev-parse":
		if args[1] == "HEAD" {
			return []byte(g.head), nil, nil
		}
		return []byte(g.remote), nil, nil
	case "diff":
		if g.diffFiles != "" {
			return []byte(g.diffFiles), nil, nil
		}
		return []byte("docker-compose.yml"), nil, nil
	case "checkout":
		g.head = args[1]
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected git command: %v", args)
}

type fakeCompose struct {
	loadErr error
	upErrs  []error
	onUp    func(call int)
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

func (f *fakeCompose) Up(ctx context.Context, _ *ctypes.Project) error {
	f.upCalls++
	if f.onUp != nil {
		f.onUp(f.upCalls)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	if i := f.upCalls - 1; i < len(f.upErrs) {
		return f.upErrs[i]
	}
	return nil
}

func (f *fakeCompose) Down(context.Context, string) error { return nil }

// fakeSnapshotter returns snapshots[i] on the i-th call, clamped to the
// last entry once exhausted.
type fakeSnapshotter struct {
	snapshots []health.Snapshot
	err       error
	call      int
}

func (f *fakeSnapshotter) Snapshot(context.Context, string) (health.Snapshot, error) {
	if f.err != nil {
		return health.Snapshot{}, f.err
	}
	i := min(f.call, len(f.snapshots)-1)
	f.call++
	return f.snapshots[i], nil
}

func snapshots(s ...health.Snapshot) *fakeSnapshotter {
	return &fakeSnapshotter{snapshots: s}
}

func healthySnapshot() health.Snapshot {
	return health.Snapshot{Containers: []health.ContainerStatus{
		{ID: "c1", Service: "web", State: health.StateRunning},
	}}
}

func unhealthySnapshot() health.Snapshot {
	return health.Snapshot{Containers: []health.ContainerStatus{
		{ID: "c1", Service: "web", State: health.StateExited, ExitCode: 1},
	}}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) bool {
	c.now = c.now.Add(d)
	return true
}

type cancelingClock struct {
	fakeClock
	sleeps   int
	cancelAt int
	cancel   context.CancelFunc
}

func (c *cancelingClock) Sleep(ctx context.Context, d time.Duration) bool {
	c.sleeps++
	if c.sleeps == c.cancelAt {
		c.cancel()
		return false
	}
	return c.fakeClock.Sleep(ctx, d)
}

func testDeps(t *testing.T, g *fakeGit, compose ComposeService, snap health.Snapshotter, st *state.State) (Deps, *state.MemStore) {
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
	store := &state.MemStore{State: st}
	return Deps{
		Config:   cfg,
		Git:      &git.Syncer{Runner: g, RepoPath: cfg.RepoPath, Remote: cfg.Remote, Branch: cfg.Branch},
		Compose:  compose,
		Health:   snap,
		Clock:    &fakeClock{},
		State:    store,
		Notifier: notify.New("", slog.New(slog.DiscardHandler)),
		Log:      slog.New(slog.DiscardHandler),
	}, store
}

func gitNoChange(commit string) *fakeGit {
	return &fakeGit{head: commit, remote: commit}
}

func gitChange(old, new string) *fakeGit {
	return &fakeGit{head: old, remote: new}
}

func withHealthy(commit string) *state.State {
	st := state.New()
	st.LastHealthyCommit = commit
	return st
}

// ---- tests ----

func TestReconcileNoChange(t *testing.T) {
	compose := &fakeCompose{}
	deps, _ := testDeps(t, gitNoChange("abc123"), compose, snapshots(healthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Changed {
		t.Error("expected Changed = false")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
}

func TestReconcileComposeFileUnchanged(t *testing.T) {
	compose := &fakeCompose{}
	g := gitChange("abc123", "def456")
	g.diffFiles = "README.md"
	deps, _ := testDeps(t, g, compose, snapshots(healthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Skipped {
		t.Error("expected Skipped = true")
	}
	if result.Applied {
		t.Error("expected Applied = false")
	}
	if result.Notification != nil {
		t.Error("expected no notification")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
}

func TestReconcileComposeFileChangedAmongOthers(t *testing.T) {
	compose := &fakeCompose{}
	g := gitChange("abc123", "def456")
	g.diffFiles = "README.md\ndocker-compose.yml"
	deps, _ := testDeps(t, g, compose, snapshots(healthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Skipped {
		t.Error("expected Skipped = false")
	}
	if !result.Applied {
		t.Error("expected Applied = true")
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1", compose.upCalls)
	}
}

func TestReconcileApplySucceedsPromotesState(t *testing.T) {
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), state.New())

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
	if g.head != "new222" {
		t.Errorf("HEAD = %q, want %q", g.head, "new222")
	}
	st := store.State
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
	st := withHealthy("old111")
	st.LastFailedCommit = "new222"
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	deps, _ := testDeps(t, g, compose, snapshots(healthySnapshot()), st)

	for range 2 {
		result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
		if !result.Skipped {
			t.Fatalf("expected Skipped = true, result = %+v", result)
		}
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0 (known-bad commit)", compose.upCalls)
	}
	if g.head != "old111" {
		t.Errorf("HEAD = %q, want %q (known-bad commit must not be checked out)", g.head, "old111")
	}
}

func TestReconcileKnownBadCommitForced(t *testing.T) {
	st := state.New()
	st.LastFailedCommit = "new222"
	compose := &fakeCompose{}
	deps, store := testDeps(t, gitChange("old111", "new222"), compose, snapshots(healthySnapshot()), st)

	result := Reconcile(t.Context(), Options{Trigger: "cli", Force: true}, deps)

	if result.Skipped {
		t.Fatal("expected Skipped = false when --force is set")
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (forced past known-bad guard)", compose.upCalls)
	}
	if store.State.LastFailedCommit != "" {
		t.Errorf("LastFailedCommit = %q, want it cleared after a healthy forced apply", store.State.LastFailedCommit)
	}
}

func TestReconcilePreflightUnhealthyRevertsWithoutApplying(t *testing.T) {
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	// call0: pre-flight (unhealthy) -> triggers revert.
	// call1: revert's watch baseline+poll -> healthy.
	deps, _ := testDeps(t, g, compose, snapshots(unhealthySnapshot(), healthySnapshot()), withHealthy("old111"))

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
	if g.head != "old111" {
		t.Errorf("HEAD = %q, want %q", g.head, "old111")
	}
}

func TestReconcilePreflightSnapshotErrorRetriesNextRun(t *testing.T) {
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	snap := snapshots(healthySnapshot())
	snap.err = errors.New("docker daemon unreachable")
	deps, _ := testDeps(t, g, compose, snap, withHealthy("old111"))

	first := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)
	if first.Err == nil {
		t.Fatal("expected a pre-flight error")
	}
	if g.head != "old111" {
		t.Errorf("HEAD = %q after failed pre-flight, want %q", g.head, "old111")
	}

	snap.err = nil
	second := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)
	if !second.Applied {
		t.Fatalf("expected new222 to be applied once Docker is reachable again, result = %+v", second)
	}
}

func TestReconcileMissingEnvFileAbortsBeforeUp(t *testing.T) {
	dir := t.TempDir()
	compose := &fakeCompose{
		project: &ctypes.Project{
			Name:       "test-stack",
			WorkingDir: dir,
			Services: ctypes.Services{
				"web": ctypes.ServiceConfig{Name: "web", EnvFiles: []ctypes.EnvFile{{Path: "app.env"}}},
			},
		},
	}
	g := gitChange("old111", "new222")
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("old111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err == nil {
		t.Fatal("expected error for missing env_file")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
	if store.State.Pending() {
		t.Error("expected state unchanged (no pending_commit) on env_file failure")
	}
	if g.head != "old111" {
		t.Errorf("HEAD = %q, want checkout restored to %q", g.head, "old111")
	}

	if err := os.WriteFile(filepath.Join(dir, "app.env"), nil, 0o600); err != nil {
		t.Fatalf("writing env file: %v", err)
	}
	retry := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)
	if !retry.Applied {
		t.Fatalf("expected new222 to be applied after the env file was created, result = %+v", retry)
	}
}

func TestReconcileApplyFailsNoBaselineDegrades(t *testing.T) {
	compose := &fakeCompose{upErrs: []error{errors.New("image pull failed")}}
	deps, store := testDeps(t, gitChange("old111", "new222"), compose, snapshots(healthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Degraded {
		t.Fatalf("expected Degraded = true, result = %+v", result)
	}
	st := store.State
	if st.LastResult != state.ResultDegraded {
		t.Errorf("LastResult = %q, want %q", st.LastResult, state.ResultDegraded)
	}
	if st.LastFailedCommit != "new222" {
		t.Errorf("LastFailedCommit = %q, want %q", st.LastFailedCommit, "new222")
	}
	if !st.Pending() {
		t.Error("expected pending_commit to stay set for visibility")
	}
}

func TestReconcileApplyFailsRevertsImmediately(t *testing.T) {
	g := gitChange("old111", "new222")
	compose := &fakeCompose{upErrs: []error{errors.New("pull access denied")}}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("old111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if compose.upCalls != 2 {
		t.Errorf("Up called %d times, want 2 (failed apply + revert apply)", compose.upCalls)
	}
	st := store.State
	if st.LastHealthyCommit != "old111" {
		t.Errorf("LastHealthyCommit = %q, want unchanged %q", st.LastHealthyCommit, "old111")
	}
	if st.LastFailedCommit != "new222" {
		t.Errorf("LastFailedCommit = %q, want %q", st.LastFailedCommit, "new222")
	}
	if st.Pending() {
		t.Error("expected pending_commit cleared after a successful revert")
	}
	if g.head != "old111" {
		t.Errorf("HEAD = %q, want %q", g.head, "old111")
	}

	next := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if !next.Skipped {
		t.Errorf("expected the failed commit to be skipped on the next run, result = %+v", next)
	}
}

func TestReconcileUnhealthyWatchRevertsSuccessfully(t *testing.T) {
	compose := &fakeCompose{}
	// call0: pre-flight (healthy). call1: apply-watch baseline (healthy).
	// call2: apply-watch poll (exited -> fails). call3: revert-watch
	// baseline (healthy). call4: revert-watch poll (healthy -> passes).
	snap := snapshots(healthySnapshot(), healthySnapshot(), unhealthySnapshot(), healthySnapshot(), healthySnapshot())
	deps, store := testDeps(t, gitChange("old111", "new222"), compose, snap, withHealthy("old111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	st := store.State
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
	compose := &fakeCompose{}
	// pre-flight: healthy. apply watch: exited. revert watch: exited too.
	snap := snapshots(healthySnapshot(), unhealthySnapshot(), unhealthySnapshot())
	deps, store := testDeps(t, gitChange("old111", "new222"), compose, snap, withHealthy("old111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Degraded {
		t.Fatalf("expected Degraded = true, result = %+v", result)
	}
	if store.State.LastResult != state.ResultDegraded {
		t.Errorf("LastResult = %q, want %q", store.State.LastResult, state.ResultDegraded)
	}
	if !store.State.Pending() {
		t.Error("expected pending_commit to stay set for visibility")
	}

	// Subsequent run without --force must refuse to touch anything further.
	compose.upCalls = 0
	result2 := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if !result2.Degraded {
		t.Fatalf("expected still Degraded without --force, result = %+v", result2)
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times on degraded retry without --force, want 0", compose.upCalls)
	}
}

func TestReconcilePreflightDegradeIsSticky(t *testing.T) {
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	deps, _ := testDeps(t, g, compose, snapshots(unhealthySnapshot()), state.New())

	first := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)
	if !first.Degraded {
		t.Fatalf("expected Degraded = true, result = %+v", first)
	}

	second := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if !second.Degraded {
		t.Fatalf("expected DEGRADED to persist without --force, result = %+v", second)
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
	if g.head != "old111" {
		t.Errorf("HEAD = %q, want %q", g.head, "old111")
	}
}

func TestReconcileInterruptedRevertUpDoesNotDegrade(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	compose := &fakeCompose{onUp: func(call int) {
		if call == 2 {
			cancel()
		}
	}}
	snap := snapshots(healthySnapshot(), healthySnapshot(), unhealthySnapshot(), healthySnapshot())
	deps, store := testDeps(t, gitChange("old111", "new222"), compose, snap, withHealthy("old111"))

	result := Reconcile(ctx, Options{Trigger: "poll"}, deps)

	if result.Degraded {
		t.Fatalf("expected an interrupted revert not to degrade, result = %+v", result)
	}
	if store.State.LastResult == state.ResultDegraded {
		t.Fatal("interrupted revert persisted DEGRADED")
	}
	if !store.State.RevertInProgress() {
		t.Fatalf("expected the interrupted revert to be recorded, state = %+v", store.State)
	}

	resumed := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if !resumed.Reverted {
		t.Fatalf("expected the next run to finish the revert, result = %+v", resumed)
	}
	if store.State.LastHealthyCommit != "old111" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "old111")
	}
	if store.State.Pending() {
		t.Error("expected pending_commit cleared after the resumed revert")
	}
}

func TestReconcileInterruptedRevertWatchResumesRevert(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	snap := snapshots(healthySnapshot(), healthySnapshot(), unhealthySnapshot(), healthySnapshot())
	deps, store := testDeps(t, g, compose, snap, withHealthy("old111"))
	deps.Clock = &cancelingClock{cancelAt: 2, cancel: cancel}

	first := Reconcile(ctx, Options{Trigger: "poll"}, deps)
	if first.Err == nil {
		t.Fatal("expected an error from the interrupted revert watch")
	}
	if !store.State.RevertInProgress() {
		t.Fatalf("expected the interrupted revert to be recorded, state = %+v", store.State)
	}

	deps.Clock = &fakeClock{}
	second := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)
	if !second.Reverted {
		t.Fatalf("expected the next run to finish the revert, result = %+v", second)
	}
	st := store.State
	if st.LastHealthyCommit != "old111" {
		t.Errorf("LastHealthyCommit = %q, want %q (known-bad commit must never be promoted)", st.LastHealthyCommit, "old111")
	}
	if st.LastFailedCommit != "new222" {
		t.Errorf("LastFailedCommit = %q, want %q", st.LastFailedCommit, "new222")
	}
	if g.head != "old111" {
		t.Errorf("HEAD = %q, want %q", g.head, "old111")
	}
}

func TestReconcileCrashRecoveryReappliesPendingCommit(t *testing.T) {
	st := withHealthy("old111")
	st.PendingCommit = "pending333"
	g := gitNoChange("old111")
	compose := &fakeCompose{}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (pending commit is re-applied before it is watched)", compose.upCalls)
	}
	if g.head != "pending333" {
		t.Errorf("HEAD = %q, want %q", g.head, "pending333")
	}
	if store.State.LastHealthyCommit != "pending333" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "pending333")
	}
	if store.State.Pending() {
		t.Error("expected pending_commit cleared")
	}
}

func TestReconcileCrashRecoveryUnhealthyRevertsImmediately(t *testing.T) {
	st := withHealthy("old111")
	st.PendingCommit = "pending333"
	compose := &fakeCompose{}
	// live snapshot: unhealthy -> immediate revert; revert watch: healthy.
	deps, _ := testDeps(t, gitNoChange("pending333"), compose, snapshots(unhealthySnapshot(), healthySnapshot()), st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (the revert apply)", compose.upCalls)
	}
}

func TestReconcileDryRunNoSDKCallsNoStateWrites(t *testing.T) {
	g := gitChange("old111", "new222")
	compose := &fakeCompose{}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{DryRun: true, Trigger: "cli"}, deps)

	if !result.Changed {
		t.Error("expected dry run to still report Changed = true")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0 on dry run", compose.upCalls)
	}
	if store.State.PendingCommit != "" || store.State.LastHealthyCommit != "" {
		t.Errorf("expected no state writes on dry run, got %+v", store.State)
	}
	if g.head != "old111" {
		t.Errorf("HEAD = %q, want %q on dry run", g.head, "old111")
	}
}

func TestReconcileSingleFlightSkipsConcurrentRun(t *testing.T) {
	block := make(chan struct{})
	release := make(chan struct{})

	blockingSnap := &blockingSnapshotter{healthy: healthySnapshot(), block: block, release: release}
	deps, _ := testDeps(t, gitChange("old111", "new222"), &fakeCompose{}, blockingSnap, state.New())

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

func TestComposeFileChanged(t *testing.T) {
	cases := []struct {
		name         string
		repoPath     string
		composeFile  string
		changedFiles []string
		want         bool
	}{
		{
			name:         "relative compose file matches",
			repoPath:     "/repo",
			composeFile:  "docker-compose.yml",
			changedFiles: []string{"docker-compose.yml"},
			want:         true,
		},
		{
			name:         "relative compose file with ./ prefix matches",
			repoPath:     "/repo",
			composeFile:  "./docker-compose.yml",
			changedFiles: []string{"docker-compose.yml"},
			want:         true,
		},
		{
			name:         "absolute compose file matches (smoke.sh style config)",
			repoPath:     "/repo",
			composeFile:  "/repo/docker-compose.yml",
			changedFiles: []string{"docker-compose.yml"},
			want:         true,
		},
		{
			name:         "unrelated file only",
			repoPath:     "/repo",
			composeFile:  "docker-compose.yml",
			changedFiles: []string{"README.md"},
			want:         false,
		},
		{
			name:         "compose file among other changed files",
			repoPath:     "/repo",
			composeFile:  "docker-compose.yml",
			changedFiles: []string{"README.md", "docker-compose.yml"},
			want:         true,
		},
		{
			name:         "no changed files",
			repoPath:     "/repo",
			composeFile:  "docker-compose.yml",
			changedFiles: nil,
			want:         false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeFileChanged(tc.repoPath, tc.composeFile, tc.changedFiles)
			if got != tc.want {
				t.Errorf("composeFileChanged(%q, %q, %v) = %v, want %v",
					tc.repoPath, tc.composeFile, tc.changedFiles, got, tc.want)
			}
		})
	}
}
