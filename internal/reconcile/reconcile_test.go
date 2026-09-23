package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

// ---- fakes ----

type fakeGit struct {
	head       string
	remote     string
	fetchErr   error
	diffFiles  string // newline-separated for readability; emitted NUL-separated like `git diff -z`
	onCheckout func(commit string)
	subjects   map[string]string
	remoteURL  string
}

func (g *fakeGit) Run(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "fetch":
		return nil, nil, g.fetchErr
	case "rev-parse":
		if args[len(args)-1] == "HEAD" {
			return []byte(g.head), nil, nil
		}
		return []byte(g.remote), nil, nil
	case "diff":
		files := g.diffFiles
		if files == "" {
			files = "docker-compose.yml"
		}
		return []byte(strings.ReplaceAll(files, "\n", "\x00") + "\x00"), nil, nil
	case "log":
		if subject, ok := g.subjects[args[len(args)-1]]; ok {
			return []byte(subject + "\x00teyhouse"), nil, nil
		}
	case "rev-list":
		if g.subjects != nil {
			return []byte("3"), nil, nil
		}
	case "remote":
		if g.remoteURL != "" {
			return []byte(g.remoteURL), nil, nil
		}
	case "checkout":
		commit := args[len(args)-1]
		g.head = commit
		if g.onCheckout != nil {
			g.onCheckout(commit)
		}
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected git command: %v", args)
}

type loadCall struct {
	files       []string
	projectName string
}

type fakeCompose struct {
	mu         sync.Mutex
	loadErr    error
	downErr    error
	upErrs     []error
	onUp       func(call int)
	onDown     func(projectName string)
	project    *ctypes.Project
	upCalls    int
	upProjects []string
	loadCalls  []loadCall
	downCalls  []string
}

func (f *fakeCompose) LoadProject(_ context.Context, files []string, projectName string) (*ctypes.Project, error) {
	f.loadCalls = append(f.loadCalls, loadCall{files: files, projectName: projectName})
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if f.project != nil {
		return f.project, nil
	}
	return &ctypes.Project{Name: projectName, Services: ctypes.Services{"web": ctypes.ServiceConfig{Name: "web"}}}, nil
}

func (f *fakeCompose) Up(ctx context.Context, project *ctypes.Project) error {
	f.upCalls++
	if project != nil {
		f.upProjects = append(f.upProjects, project.Name)
	}
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

func (f *fakeCompose) Down(_ context.Context, projectName string) error {
	if f.onDown != nil {
		f.onDown(projectName)
	}
	f.mu.Lock()
	f.downCalls = append(f.downCalls, projectName)
	f.mu.Unlock()
	return f.downErr
}

// fakeSnapshotter returns snapshots[i] on the i-th call, clamped to the
// last entry once exhausted.
type fakeSnapshotter struct {
	mu        sync.Mutex
	snapshots []health.Snapshot
	err       error
	call      int
}

func (f *fakeSnapshotter) Snapshot(context.Context, string) (health.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

type perProjectSnapshotter struct {
	mu   sync.Mutex
	byID map[string]*fakeSnapshotter
}

func (p *perProjectSnapshotter) set(projectName string, snap *fakeSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID == nil {
		p.byID = map[string]*fakeSnapshotter{}
	}
	p.byID[projectName] = snap
}

func (p *perProjectSnapshotter) Snapshot(ctx context.Context, projectName string) (health.Snapshot, error) {
	p.mu.Lock()
	snap := p.byID[projectName]
	p.mu.Unlock()
	if snap == nil {
		return health.Snapshot{}, fmt.Errorf("unexpected Snapshot call for project %q", projectName)
	}
	return snap.Snapshot(ctx, projectName)
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

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
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

const validComposeYAML = `
services:
  web:
    image: nginx:alpine
`

const invalidComposeYAML = `
services: web
`

func writeComposeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testDirDeps(t *testing.T, g *fakeGit, compose ComposeService, snap health.Snapshotter, st *state.State, repoPath, composeDir string) (Deps, *state.MemStore) {
	t.Helper()
	cfg := &config.Config{
		RepoPath:                  repoPath,
		Remote:                    "origin",
		Branch:                    "main",
		ComposeDir:                composeDir,
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

// ---- tests ----

func TestReconcileNoChange(t *testing.T) {
	compose := &fakeCompose{}
	deps, _ := testDeps(t, gitNoChange("abc123"), compose, snapshots(healthySnapshot()), withHealthy("abc123"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Changed {
		t.Error("expected Changed = false")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
}

func TestReconcileFreshInstallAppliesWithoutANewCommit(t *testing.T) {
	compose := &fakeCompose{}
	deps, store := testDeps(t, gitNoChange("abc123"), compose, snapshots(healthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected the first run of an empty state to deploy, result = %+v", result)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1", compose.upCalls)
	}
	if store.State.LastHealthyCommit != "abc123" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "abc123")
	}
}

func TestReconcileComposeFileUnchanged(t *testing.T) {
	compose := &fakeCompose{}
	g := gitChange("abc123", "def456")
	g.diffFiles = "README.md"
	deps, _ := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("abc123"))

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
	g := gitChange("aaa111", "bbb222")
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
	if g.head != "bbb222" {
		t.Errorf("HEAD = %q, want %q", g.head, "bbb222")
	}
	st := store.State
	if st.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q", st.LastHealthyCommit, "bbb222")
	}
	if st.Pending() {
		t.Error("expected pending_commit cleared after promotion")
	}
	if st.LastResult != state.ResultSuccess {
		t.Errorf("LastResult = %q, want %q", st.LastResult, state.ResultSuccess)
	}
}

func TestReconcileKnownBadCommitSkipped(t *testing.T) {
	st := withHealthy("aaa111")
	st.LastFailedCommit = "bbb222"
	g := gitChange("aaa111", "bbb222")
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
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q, want %q (known-bad commit must not be checked out)", g.head, "aaa111")
	}
}

func TestReconcileKnownBadCommitForced(t *testing.T) {
	st := state.New()
	st.LastFailedCommit = "bbb222"
	compose := &fakeCompose{}
	deps, store := testDeps(t, gitChange("aaa111", "bbb222"), compose, snapshots(healthySnapshot()), st)

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
	g := gitChange("aaa111", "bbb222")
	compose := &fakeCompose{}
	// call0: pre-flight (unhealthy) -> triggers revert.
	// call1: revert's watch baseline+poll -> healthy.
	deps, _ := testDeps(t, g, compose, snapshots(unhealthySnapshot(), healthySnapshot()), withHealthy("aaa111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (only the revert apply, not the new commit)", compose.upCalls)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if result.RolledBackTo != "aaa111" {
		t.Errorf("RolledBackTo = %q, want %q", result.RolledBackTo, "aaa111")
	}
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q, want %q", g.head, "aaa111")
	}
}

func TestReconcilePreflightSnapshotErrorRetriesNextRun(t *testing.T) {
	g := gitChange("aaa111", "bbb222")
	compose := &fakeCompose{}
	snap := snapshots(healthySnapshot())
	snap.err = errors.New("docker daemon unreachable")
	deps, _ := testDeps(t, g, compose, snap, withHealthy("aaa111"))

	first := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)
	if first.Err == nil {
		t.Fatal("expected a pre-flight error")
	}
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q after failed pre-flight, want %q", g.head, "aaa111")
	}

	snap.err = nil
	second := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)
	if !second.Applied {
		t.Fatalf("expected bbb222 to be applied once Docker is reachable again, result = %+v", second)
	}
}

func TestReconcileMissingEnvFileAbortsBeforeUp(t *testing.T) {
	dir := t.TempDir()
	compose := &fakeCompose{
		project: &ctypes.Project{
			Name:       "test-stack",
			WorkingDir: dir,
			Services: ctypes.Services{
				"web": ctypes.ServiceConfig{Name: "web", EnvFiles: []ctypes.EnvFile{{Path: "app.env", Required: true}}},
			},
		},
	}
	g := gitChange("aaa111", "bbb222")
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"))

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
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q, want checkout restored to %q", g.head, "aaa111")
	}

	if err := os.WriteFile(filepath.Join(dir, "app.env"), nil, 0o600); err != nil {
		t.Fatalf("writing env file: %v", err)
	}
	skipped := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)
	if !skipped.Skipped || skipped.Applied {
		t.Fatalf("expected bbb222 to be skipped as known-bad instead of retried every tick, result = %+v", skipped)
	}
	if store.State.LastResult != state.ResultSkippedKnownBad {
		t.Errorf("LastResult = %q, want %q", store.State.LastResult, state.ResultSkippedKnownBad)
	}

	retry := Reconcile(t.Context(), Options{Trigger: "cli", Force: true}, deps)
	if !retry.Applied {
		t.Fatalf("expected --force to apply bbb222 after the env file was created, result = %+v", retry)
	}
}

func TestReconcileApplyFailsNoBaselineDegrades(t *testing.T) {
	compose := &fakeCompose{upErrs: []error{errors.New("image pull failed")}}
	deps, store := testDeps(t, gitChange("aaa111", "bbb222"), compose, snapshots(healthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Degraded {
		t.Fatalf("expected Degraded = true, result = %+v", result)
	}
	st := store.State
	if st.LastResult != state.ResultDegraded {
		t.Errorf("LastResult = %q, want %q", st.LastResult, state.ResultDegraded)
	}
	if st.LastFailedCommit != "bbb222" {
		t.Errorf("LastFailedCommit = %q, want %q", st.LastFailedCommit, "bbb222")
	}
	if st.Pending() {
		t.Error("expected the pending block cleared: a DEGRADED run must not route the next --force into crash recovery")
	}
	if st.LastAttemptCommit != "bbb222" {
		t.Errorf("LastAttemptCommit = %q, want %q to keep the stuck commit visible", st.LastAttemptCommit, "bbb222")
	}
}

func TestReconcileApplyFailsRevertsImmediately(t *testing.T) {
	g := gitChange("aaa111", "bbb222")
	compose := &fakeCompose{upErrs: []error{errors.New("pull access denied")}}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if compose.upCalls != 2 {
		t.Errorf("Up called %d times, want 2 (failed apply + revert apply)", compose.upCalls)
	}
	st := store.State
	if st.LastHealthyCommit != "aaa111" {
		t.Errorf("LastHealthyCommit = %q, want unchanged %q", st.LastHealthyCommit, "aaa111")
	}
	if st.LastFailedCommit != "bbb222" {
		t.Errorf("LastFailedCommit = %q, want %q", st.LastFailedCommit, "bbb222")
	}
	if st.Pending() {
		t.Error("expected pending_commit cleared after a successful revert")
	}
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q, want %q", g.head, "aaa111")
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
	deps, store := testDeps(t, gitChange("aaa111", "bbb222"), compose, snap, withHealthy("aaa111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	st := store.State
	if st.LastHealthyCommit != "aaa111" {
		t.Errorf("LastHealthyCommit = %q, want unchanged %q", st.LastHealthyCommit, "aaa111")
	}
	if st.LastFailedCommit != "bbb222" {
		t.Errorf("LastFailedCommit = %q, want %q (guard should keep skipping it)", st.LastFailedCommit, "bbb222")
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
	deps, store := testDeps(t, gitChange("aaa111", "bbb222"), compose, snap, withHealthy("aaa111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Degraded {
		t.Fatalf("expected Degraded = true, result = %+v", result)
	}
	if store.State.LastResult != state.ResultDegraded {
		t.Errorf("LastResult = %q, want %q", store.State.LastResult, state.ResultDegraded)
	}
	if store.State.Pending() {
		t.Error("expected the pending block cleared: a DEGRADED run must not route the next --force into crash recovery")
	}
	if store.State.LastAttemptCommit != "bbb222" {
		t.Errorf("LastAttemptCommit = %q, want %q to keep the stuck commit visible", store.State.LastAttemptCommit, "bbb222")
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
	g := gitChange("aaa111", "bbb222")
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
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q, want %q", g.head, "aaa111")
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
	deps, store := testDeps(t, gitChange("aaa111", "bbb222"), compose, snap, withHealthy("aaa111"))

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
	if store.State.LastHealthyCommit != "aaa111" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "aaa111")
	}
	if store.State.Pending() {
		t.Error("expected pending_commit cleared after the resumed revert")
	}
}

func TestReconcileInterruptedRevertWatchResumesRevert(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	g := gitChange("aaa111", "bbb222")
	compose := &fakeCompose{}
	snap := snapshots(healthySnapshot(), healthySnapshot(), unhealthySnapshot(), healthySnapshot())
	deps, store := testDeps(t, g, compose, snap, withHealthy("aaa111"))
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
	if st.LastHealthyCommit != "aaa111" {
		t.Errorf("LastHealthyCommit = %q, want %q (known-bad commit must never be promoted)", st.LastHealthyCommit, "aaa111")
	}
	if st.LastFailedCommit != "bbb222" {
		t.Errorf("LastFailedCommit = %q, want %q", st.LastFailedCommit, "bbb222")
	}
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q, want %q", g.head, "aaa111")
	}
}

func TestReconcileCrashRecoveryReappliesPendingCommit(t *testing.T) {
	st := withHealthy("aaa111")
	st.PendingCommit = "ccc333"
	g := gitNoChange("ccc333")
	compose := &fakeCompose{}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (pending commit is re-applied before it is watched)", compose.upCalls)
	}
	if g.head != "ccc333" {
		t.Errorf("HEAD = %q, want %q", g.head, "ccc333")
	}
	if store.State.LastHealthyCommit != "ccc333" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "ccc333")
	}
	if store.State.Pending() {
		t.Error("expected pending_commit cleared")
	}
}

func TestReconcileCrashRecoveryUnhealthyRevertsImmediately(t *testing.T) {
	st := withHealthy("aaa111")
	st.PendingCommit = "ccc333"
	compose := &fakeCompose{}
	// live snapshot: unhealthy -> immediate revert; revert watch: healthy.
	deps, _ := testDeps(t, gitNoChange("ccc333"), compose, snapshots(unhealthySnapshot(), healthySnapshot()), st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (the revert apply)", compose.upCalls)
	}
}

func TestReconcileDryRunNoSDKCallsNoStateWrites(t *testing.T) {
	g := gitChange("aaa111", "bbb222")
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
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q, want %q on dry run", g.head, "aaa111")
	}
}

func TestReconcileSingleFlightSkipsConcurrentRun(t *testing.T) {
	block := make(chan struct{})
	release := make(chan struct{})

	blockingSnap := &blockingSnapshotter{healthy: healthySnapshot(), block: block, release: release}
	deps, _ := testDeps(t, gitChange("aaa111", "bbb222"), &fakeCompose{}, blockingSnap, state.New())

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

func TestReconcileDirModeMultiStackAllHealthy(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/service-a.yaml\ndeployment/db/docker-compose.yaml"
	compose := &fakeCompose{}
	deps, store := testDirDeps(t, g, compose, snapshots(healthySnapshot()), state.New(), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected Applied = true, result = %+v", result)
	}
	if compose.upCalls != 2 {
		t.Errorf("Up called %d times, want 2 (one per stack)", compose.upCalls)
	}
	gotStacks := append([]string(nil), result.Stacks...)
	sort.Strings(gotStacks)
	if want := []string{"test-stack", "test-stack-db"}; !equalStringSlices(gotStacks, want) {
		t.Errorf("Stacks = %v, want %v", gotStacks, want)
	}
	if store.State.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "bbb222")
	}
}

func TestReconcileDirModePartialChangeOnlyAppliesChangedStack(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/db/docker-compose.yaml"
	compose := &fakeCompose{}
	deps, _ := testDirDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected Applied = true, result = %+v", result)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 (root stack must not be touched)", compose.upCalls)
	}
	if !equalStringSlices(compose.upProjects, []string{"test-stack-db"}) {
		t.Errorf("upProjects = %v, want only test-stack-db applied", compose.upProjects)
	}
	if !equalStringSlices(result.Stacks, []string{"test-stack-db"}) {
		t.Errorf("Stacks = %v, want only test-stack-db in the cycle", result.Stacks)
	}
}

func TestReconcileDirModeOneStackFailureRevertsOnlyThatCycle(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "c", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/service-a.yaml"
	compose := &fakeCompose{}

	perProject := &perProjectSnapshotter{}
	perProject.set("test-stack", snapshots(healthySnapshot(), healthySnapshot(), unhealthySnapshot(), healthySnapshot(), healthySnapshot()))
	perProject.set("test-stack-c", snapshots(healthySnapshot()))

	deps, store := testDirDeps(t, g, compose, perProject, withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if slices.Contains(compose.upProjects, "test-stack-c") || slices.Contains(compose.downCalls, "test-stack-c") {
		t.Fatalf("test-stack-c must never be applied or torn down by another stack's failure, up = %v down = %v",
			compose.upProjects, compose.downCalls)
	}
	if compose.upCalls != 2 {
		t.Errorf("Up called %d times, want 2 (failed apply + revert apply of test-stack only)", compose.upCalls)
	}
	if store.State.LastHealthyCommit != "aaa111" {
		t.Errorf("LastHealthyCommit = %q, want unchanged %q", store.State.LastHealthyCommit, "aaa111")
	}
}

func TestReconcileDirModeInvalidFileFailsLoudly(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "broken.yaml"), invalidComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/service-a.yaml"
	compose := &fakeCompose{}
	deps, _ := testDirDeps(t, g, compose, snapshots(healthySnapshot()), state.New(), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err == nil {
		t.Fatal("expected an error for the invalid compose file")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0 (must fail before touching Docker)", compose.upCalls)
	}
}

func TestVanishedStacksTearDownOnlyRemoved(t *testing.T) {
	compose := &fakeCompose{}
	deps := Deps{Compose: compose, Log: slog.New(slog.DiscardHandler)}

	previous := stackFixtures("test-stack", "test-stack-db", "test-stack-old")
	current := stackFixtures("test-stack", "test-stack-db")

	tearDownStacks(t.Context(), deps, vanishedStacks(previous, current), "removed")

	if len(compose.downCalls) != 1 || compose.downCalls[0] != "test-stack-old" {
		t.Errorf("downCalls = %v, want exactly [test-stack-old]", compose.downCalls)
	}
}

func stackFixtures(projectNames ...string) []compose.Stack {
	stacks := make([]compose.Stack, len(projectNames))
	for i, name := range projectNames {
		stacks[i] = compose.Stack{ProjectName: name}
	}
	return stacks
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReconcileDirModeRevertUsesRollbackTargetFileList(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	added := filepath.Join(composeDir, "service-b.yaml")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/service-a.yaml\ndeployment/service-b.yaml"
	g.onCheckout = func(commit string) {
		switch commit {
		case "bbb222":
			writeComposeFile(t, added, validComposeYAML)
		case "aaa111":
			if err := os.Remove(added); err != nil {
				t.Error(err)
			}
		}
	}

	compose := &fakeCompose{}
	perProject := &perProjectSnapshotter{}
	perProject.set("test-stack", snapshots(healthySnapshot(), healthySnapshot(), unhealthySnapshot(), healthySnapshot(), healthySnapshot()))
	deps, _ := testDirDeps(t, g, compose, perProject, withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted || result.Degraded {
		t.Fatalf("expected a clean revert, result = %+v", result)
	}
	if len(compose.loadCalls) != 2 {
		t.Fatalf("loadCalls = %+v, want one for the apply and one for the revert", compose.loadCalls)
	}
	revertFiles := compose.loadCalls[1].files
	for _, f := range revertFiles {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("revert loaded %s, which does not exist at the rollback target: %v", f, err)
		}
	}
	if len(revertFiles) != 1 {
		t.Errorf("revert files = %v, want only the file present at the rollback target", revertFiles)
	}
}

func TestReconcileDirModeDeletedComposeFileReappliesStack(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	removed := filepath.Join(composeDir, "service-b.yaml")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, removed, validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/service-b.yaml"
	g.onCheckout = func(commit string) {
		if commit == "bbb222" {
			if err := os.Remove(removed); err != nil {
				t.Error(err)
			}
		}
	}

	compose := &fakeCompose{}
	deps, store := testDirDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected the stack to be re-applied without its deleted file, result = %+v", result)
	}
	if len(compose.loadCalls) != 1 || len(compose.loadCalls[0].files) != 1 {
		t.Errorf("loadCalls = %+v, want one call with only the surviving file", compose.loadCalls)
	}
	if store.State.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "bbb222")
	}
}

func TestReconcileDirModeRemovedStackTornDownAndCommitRecorded(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/db/docker-compose.yaml"
	g.onCheckout = func(commit string) {
		if commit == "bbb222" {
			if err := os.RemoveAll(filepath.Join(composeDir, "db")); err != nil {
				t.Error(err)
			}
		}
	}

	compose := &fakeCompose{}
	deps, store := testDirDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err != nil || result.Applied {
		t.Fatalf("expected a clean no-apply cycle, result = %+v", result)
	}
	if !result.Skipped {
		t.Errorf("expected Skipped = true when no stack changed")
	}
	if len(compose.downCalls) != 1 || compose.downCalls[0] != "test-stack-db" {
		t.Errorf("downCalls = %v, want exactly [test-stack-db]", compose.downCalls)
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
	if store.State.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q (the checkout moved)", store.State.LastHealthyCommit, "bbb222")
	}
}

func TestReconcileDirModeRemovedStackSurvivesRevert(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/service-a.yaml\ndeployment/db/docker-compose.yaml"
	g.onCheckout = func(commit string) {
		switch commit {
		case "bbb222":
			if err := os.RemoveAll(filepath.Join(composeDir, "db")); err != nil {
				t.Error(err)
			}
		case "aaa111":
			writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)
		}
	}

	compose := &fakeCompose{}
	perProject := &perProjectSnapshotter{}
	perProject.set("test-stack", snapshots(healthySnapshot(), healthySnapshot(), unhealthySnapshot(), healthySnapshot(), healthySnapshot()))
	perProject.set("test-stack-db", snapshots(healthySnapshot()))
	deps, _ := testDirDeps(t, g, compose, perProject, withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted {
		t.Fatalf("expected Reverted = true, result = %+v", result)
	}
	if len(compose.downCalls) != 0 {
		t.Errorf("downCalls = %v, want none: the rollback target still has that stack", compose.downCalls)
	}
}

func TestReconcileDirModeNewStackTornDownOnRevert(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/new/docker-compose.yaml"
	g.onCheckout = func(commit string) {
		switch commit {
		case "bbb222":
			writeComposeFile(t, filepath.Join(composeDir, "new", "docker-compose.yaml"), validComposeYAML)
		case "aaa111":
			if err := os.RemoveAll(filepath.Join(composeDir, "new")); err != nil {
				t.Error(err)
			}
		}
	}

	compose := &fakeCompose{}
	perProject := &perProjectSnapshotter{}
	perProject.set("test-stack", snapshots(healthySnapshot()))
	perProject.set("test-stack-new", snapshots(healthySnapshot(), unhealthySnapshot()))
	deps, _ := testDirDeps(t, g, compose, perProject, withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Reverted || result.Degraded {
		t.Fatalf("expected a clean revert, result = %+v", result)
	}
	if len(compose.downCalls) != 1 || compose.downCalls[0] != "test-stack-new" {
		t.Errorf("downCalls = %v, want exactly [test-stack-new]", compose.downCalls)
	}
}

func TestReconcileDirModePreflightChecksEveryStack(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/service-a.yaml"
	compose := &fakeCompose{}

	perProject := &perProjectSnapshotter{}
	perProject.set("test-stack", snapshots(healthySnapshot()))
	perProject.set("test-stack-db", snapshots(unhealthySnapshot(), healthySnapshot()))

	deps, store := testDirDeps(t, g, compose, perProject, withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Applied {
		t.Fatalf("expected the pre-flight gate to block the apply, result = %+v", result)
	}
	if !result.Reverted {
		t.Errorf("expected the pre-flight gate to revert, result = %+v", result)
	}
	if store.State.LastHealthyCommit != "aaa111" {
		t.Errorf("LastHealthyCommit = %q, want unchanged %q", store.State.LastHealthyCommit, "aaa111")
	}
}

func TestReconcileDirModeIgnoresNonComposeYAML(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "db", "prometheus.yml"), "global:\n  scrape_interval: 15s\n")

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/db/prometheus.yml"
	compose := &fakeCompose{}
	deps, _ := testDirDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if len(compose.loadCalls) != 1 || len(compose.loadCalls[0].files) != 1 {
		t.Fatalf("loadCalls = %+v, want one call with only the compose file", compose.loadCalls)
	}
	if !strings.HasSuffix(compose.loadCalls[0].files[0], "docker-compose.yaml") {
		t.Errorf("loaded %q, want only the compose file", compose.loadCalls[0].files[0])
	}
}

func TestWatchStacksReportsEveryFailingStackAndStopsEarly(t *testing.T) {
	perProject := &perProjectSnapshotter{}
	perProject.set("stack-a", snapshots(healthySnapshot()))
	perProject.set("stack-b", snapshots(healthySnapshot(), unhealthySnapshot()))

	deps, _ := testDirDeps(t, gitChange("aaa111", "bbb222"), &fakeCompose{}, perProject, state.New(), t.TempDir(), t.TempDir())

	result, _, err := watchStacks(t.Context(), deps, stackFixtures("stack-a", "stack-b"), nil, "bbb222")
	if err != nil {
		t.Fatalf("watchStacks: %v", err)
	}
	if result.Outcome != health.Unhealthy {
		t.Fatalf("Outcome = %v, want unhealthy", result.Outcome)
	}
	if !strings.Contains(result.Reason, "stack-b") {
		t.Errorf("Reason = %q, want it to name the failing stack", result.Reason)
	}
	for _, f := range result.Failures {
		if !strings.HasPrefix(f, "stack-b: ") {
			t.Errorf("failure %q is not attributed to its stack", f)
		}
	}
}

func TestWatchStacksNoStacksIsHealthy(t *testing.T) {
	deps, _ := testDirDeps(t, gitChange("aaa111", "bbb222"), &fakeCompose{}, snapshots(healthySnapshot()), state.New(), t.TempDir(), t.TempDir())

	result, _, err := watchStacks(t.Context(), deps, nil, nil, "bbb222")
	if err != nil {
		t.Fatalf("watchStacks: %v", err)
	}
	if result.Outcome != health.Healthy {
		t.Errorf("Outcome = %v, want healthy", result.Outcome)
	}
}

func TestReconcileIrrelevantChangeAdvancesTheCheckout(t *testing.T) {
	compose := &fakeCompose{}
	g := gitChange("abc123", "def456")
	g.diffFiles = "README.md"
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("abc123"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Skipped {
		t.Error("expected Skipped = true")
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
	if g.head != "def456" {
		t.Errorf("HEAD = %q, want the checkout to advance to %q so it stops re-diffing forever", g.head, "def456")
	}
	if store.State.LastHealthyCommit != "abc123" {
		t.Errorf("last_healthy_commit = %q, want the rollback target to stay at the commit that passed a watch (%q)", store.State.LastHealthyCommit, "abc123")
	}
	if store.State.LastCheckoutCommit != "def456" {
		t.Errorf("last_checkout_commit = %q, want %q", store.State.LastCheckoutCommit, "def456")
	}
}

func TestReconcileReAppliesWhenTheCheckoutDriftedFromState(t *testing.T) {
	compose := &fakeCompose{}
	g := gitNoChange("def456")
	deps, _ := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("abc123"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected the drifted checkout to be re-applied, result = %+v", result)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1", compose.upCalls)
	}
}

func TestReconcileGitFailureRecordsLastResult(t *testing.T) {
	g := &fakeGit{head: "abc123", remote: "abc123", fetchErr: errors.New("network unreachable")}
	deps, store := testDeps(t, g, &fakeCompose{}, snapshots(healthySnapshot()), withHealthy("abc123"))

	if result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps); result.Err == nil {
		t.Fatal("expected a git error")
	}
	if store.State.LastResult != state.ResultFailedGit {
		t.Errorf("last_result = %q, want %q", store.State.LastResult, state.ResultFailedGit)
	}
}

func TestReconcileKnownBadSkipRecordsLastResult(t *testing.T) {
	g := gitChange("abc123", "bad456")
	st := withHealthy("abc123")
	st.LastFailedCommit = "bad456"
	deps, store := testDeps(t, g, &fakeCompose{}, snapshots(healthySnapshot()), st)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Skipped {
		t.Fatalf("expected the known-bad commit to be skipped, result = %+v", result)
	}
	if store.State.LastResult != state.ResultSkippedKnownBad {
		t.Errorf("last_result = %q, want %q", store.State.LastResult, state.ResultSkippedKnownBad)
	}
	if store.State.LastAttemptCommit != "bad456" {
		t.Errorf("last_attempt_commit = %q, want %q", store.State.LastAttemptCommit, "bad456")
	}
}

func TestReconcileFailedTeardownIsNotPromoted(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "c", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/c/docker-compose.yaml"
	g.onCheckout = func(commit string) {
		if commit == "bbb222" {
			if err := os.RemoveAll(filepath.Join(composeDir, "c")); err != nil {
				t.Error(err)
			}
		}
	}

	compose := &fakeCompose{downErr: errors.New("docker daemon unreachable")}
	perProject := &perProjectSnapshotter{}
	perProject.set("test-stack", snapshots(healthySnapshot()))
	deps, store := testDirDeps(t, g, compose, perProject, withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err == nil {
		t.Fatal("expected the failed teardown to surface as an error")
	}
	if store.State.LastHealthyCommit == "bbb222" {
		t.Error("a stack that failed to come down must not be promoted as a healthy deploy")
	}
}

func TestDegradeRecordsTheFailedCommit(t *testing.T) {
	compose := &fakeCompose{}
	g := gitChange("aaa111", "bbb222")
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot(), unhealthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Degraded {
		t.Fatalf("expected DEGRADED without a rollback target, result = %+v", result)
	}
	if store.State.LastFailedCommit != "bbb222" {
		t.Errorf("last_failed_commit = %q, want %q", store.State.LastFailedCommit, "bbb222")
	}
	if store.State.LastResult != state.ResultDegraded {
		t.Errorf("last_result = %q, want %q", store.State.LastResult, state.ResultDegraded)
	}
}

func twoServiceSnapshot(webID, dbID string) health.Snapshot {
	return health.Snapshot{Containers: []health.ContainerStatus{
		{ID: webID, Service: "web", State: health.StateRunning},
		{ID: dbID, Service: "db", State: health.StateRunning},
	}}
}

func TestReconcileReportsOnlyTheServicesThatWereRecreated(t *testing.T) {
	compose := &fakeCompose{project: &ctypes.Project{
		Name: "test-stack",
		Services: ctypes.Services{
			"web": ctypes.ServiceConfig{Name: "web"},
			"db":  ctypes.ServiceConfig{Name: "db"},
		},
	}}
	snap := snapshots(
		twoServiceSnapshot("c1", "c2"),
		twoServiceSnapshot("c9", "c2"),
		twoServiceSnapshot("c9", "c2"),
	)
	deps, _ := testDeps(t, gitChange("aaa111", "bbb222"), compose, snap, withHealthy("aaa111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected Applied = true, result = %+v", result)
	}
	if !equalStringSlices(result.Services, []string{"db", "web"}) {
		t.Errorf("Services = %v, want both services listed", result.Services)
	}
	if !equalStringSlices(result.Updated, []string{"web"}) {
		t.Errorf("Updated = %v, want only [web]: db kept its container", result.Updated)
	}
	if result.Notification == nil {
		t.Fatal("expected a success notification")
	}
	if got := embedField(t, result.Notification, "Updated"); got != "web" {
		t.Errorf("Updated field = %q, want %q", got, "web")
	}
}

func TestReconcileReportsNoUpdatedServicesWhenNothingMoved(t *testing.T) {
	compose := &fakeCompose{project: &ctypes.Project{
		Name:     "test-stack",
		Services: ctypes.Services{"web": ctypes.ServiceConfig{Name: "web"}},
	}}
	deps, _ := testDeps(t, gitChange("aaa111", "bbb222"), compose, snapshots(healthySnapshot()), withHealthy("aaa111"))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected Applied = true, result = %+v", result)
	}
	if len(result.Updated) != 0 {
		t.Errorf("Updated = %v, want none: the container was never replaced", result.Updated)
	}
	if got := embedField(t, result.Notification, "Updated"); got != "none" {
		t.Errorf("Updated field = %q, want %q", got, "none")
	}
}

func embedField(t *testing.T, embed *notify.Embed, name string) string {
	t.Helper()
	if embed == nil {
		t.Fatal("no embed")
	}
	for _, f := range embed.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	t.Fatalf("embed has no %q field, fields = %+v", name, embed.Fields)
	return ""
}
