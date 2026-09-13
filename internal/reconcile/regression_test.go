package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/state"
)

type flakyStore struct {
	inner  *state.MemStore
	failOn int
	saves  int
}

func (f *flakyStore) Load() (*state.State, error) { return f.inner.Load() }

func (f *flakyStore) Save(st *state.State) error {
	f.saves++
	if f.saves == f.failOn {
		return errors.New("no space left on device")
	}
	return f.inner.Save(st)
}

type erroringSnapshotter struct {
	mu    sync.Mutex
	snaps []health.Snapshot
	after int
	call  int
}

func (e *erroringSnapshotter) Snapshot(context.Context, string) (health.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call++
	if e.call > e.after {
		return health.Snapshot{}, errors.New("docker daemon unreachable")
	}
	return e.snaps[min(e.call-1, len(e.snaps)-1)], nil
}

func TestReconcileEnvFileChangeUnderComposeDirIsApplied(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/db/app.env"
	compose := &fakeCompose{}
	deps, store := testDirDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected an env_file change under compose_dir to be applied, result = %+v", result)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1", compose.upCalls)
	}
	if store.State.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "bbb222")
	}
}

func TestReconcileCommitRemovingTheLastStackTearsDownAndPromotes(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
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

	if result.Err != nil {
		t.Fatalf("emptying compose_dir must converge, result = %+v", result)
	}
	if len(compose.downCalls) != 1 || compose.downCalls[0] != "test-stack-db" {
		t.Errorf("downCalls = %v, want [test-stack-db]", compose.downCalls)
	}
	if store.State.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "bbb222")
	}
	if len(store.State.LastHealthyStacks) != 0 {
		t.Errorf("LastHealthyStacks = %v, want empty", store.State.LastHealthyStacks)
	}
}

func TestReconcileBadCommitIsSkippedOnTheSecondRun(t *testing.T) {
	g := gitChange("aaa111", "bbb222")
	compose := &fakeCompose{loadErr: errors.New("yaml: mapping values are not allowed")}
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"))

	if first := Reconcile(t.Context(), Options{Trigger: "poll"}, deps); first.Err == nil {
		t.Fatal("expected the invalid commit to fail")
	}
	if store.State.LastFailedCommit != "bbb222" {
		t.Fatalf("LastFailedCommit = %q, want %q so the commit is not retried every tick", store.State.LastFailedCommit, "bbb222")
	}
	if g.head != "aaa111" {
		t.Fatalf("HEAD = %q, want the checkout restored to %q", g.head, "aaa111")
	}

	second := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if !second.Skipped {
		t.Errorf("expected the second run to skip the known-bad commit, result = %+v", second)
	}
	if store.State.LastResult != state.ResultSkippedKnownBad {
		t.Errorf("LastResult = %q, want %q", store.State.LastResult, state.ResultSkippedKnownBad)
	}
	if g.head != "aaa111" {
		t.Errorf("HEAD = %q, want no further checkout churn", g.head)
	}
	if len(compose.loadCalls) != 1 {
		t.Errorf("LoadProject called %d times, want 1", len(compose.loadCalls))
	}
}

func TestReconcileFailedStateSaveLeavesNoPendingCommit(t *testing.T) {
	compose := &fakeCompose{}
	deps, store := testDeps(t, gitChange("aaa111", "bbb222"), compose, snapshots(healthySnapshot()), withHealthy("aaa111"))
	deps.State = &flakyStore{inner: store, failOn: 1}

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if result.Err == nil {
		t.Fatal("expected the failed state save to surface as an error")
	}
	if store.State.Pending() {
		t.Errorf("pending_commit = %q, want it left unset: the save that would have set it failed", store.State.PendingCommit)
	}
	if compose.upCalls != 0 {
		t.Errorf("Up called %d times, want 0", compose.upCalls)
	}
}

func TestReconcileRevertChecksEnvFilesBeforeUp(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "app.env")
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	compose := &fakeCompose{
		upErrs: []error{errors.New("image pull failed")},
		project: &ctypes.Project{
			Name:       "test-stack",
			WorkingDir: dir,
			Services: ctypes.Services{
				"web": ctypes.ServiceConfig{Name: "web", EnvFiles: []ctypes.EnvFile{{Path: "app.env", Required: true}}},
			},
		},
	}

	g := gitChange("aaa111", "bbb222")
	g.onCheckout = func(commit string) {
		if commit == "aaa111" {
			if err := os.Remove(envPath); err != nil {
				t.Error(err)
			}
		}
	}
	deps, _ := testDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"))

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if !result.Degraded {
		t.Fatalf("expected DEGRADED when the rollback target is missing its env_file, result = %+v", result)
	}
	if compose.upCalls != 1 {
		t.Errorf("Up called %d times, want 1: the revert must not reach Up with a missing env_file", compose.upCalls)
	}
	if !strings.Contains(result.Err.Error(), "env_file") {
		t.Errorf("err = %v, want it to name the missing env_file", result.Err)
	}
}

func TestReconcileInterruptedRevertStaysResumable(t *testing.T) {
	compose := &fakeCompose{}
	snap := &erroringSnapshotter{snaps: []health.Snapshot{unhealthySnapshot()}, after: 1}
	deps, store := testDeps(t, gitChange("aaa111", "bbb222"), compose, snap, withHealthy("aaa111"))

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if result.Err == nil {
		t.Fatal("expected the interrupted revert to report an error")
	}
	if result.Notification == nil {
		t.Error("expected a notification for an interrupted revert")
	}
	if !store.State.Pending() || !store.State.RevertInProgress() {
		t.Fatalf("state = %+v, want a resumable revert marker", store.State)
	}
	if store.State.LastHealthyCommit != "aaa111" {
		t.Errorf("LastHealthyCommit = %q, want the rollback target untouched", store.State.LastHealthyCommit)
	}
}

func TestReconcileCrashRecoveryTearsDownStacksRemovedSinceLastHealthy(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "a", "docker-compose.yaml"), validComposeYAML)

	st := withHealthy("aaa111")
	st.LastHealthyStacks = []string{"test-stack-a", "test-stack-b"}
	st.PendingCommit = "bbb222"

	compose := &fakeCompose{}
	deps, store := testDirDeps(t, gitNoChange("bbb222"), compose, snapshots(healthySnapshot()), st, repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if len(compose.downCalls) != 1 || compose.downCalls[0] != "test-stack-b" {
		t.Errorf("downCalls = %v, want [test-stack-b]: recovery must compare against the last healthy stack set", compose.downCalls)
	}
	if store.State.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "bbb222")
	}
}

func TestReconcilePartialTeardownIsRetriedOnTheNextRun(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "a", "docker-compose.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "b", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/a/docker-compose.yaml"
	g.onCheckout = func(commit string) {
		if commit == "bbb222" {
			if err := os.RemoveAll(filepath.Join(composeDir, "b")); err != nil {
				t.Error(err)
			}
		}
	}

	compose := &fakeCompose{downErr: errors.New("timed out waiting for containers to stop")}
	st := withHealthy("aaa111")
	st.LastHealthyStacks = []string{"test-stack-a", "test-stack-b"}
	deps, store := testDirDeps(t, g, compose, snapshots(healthySnapshot()), st, repoPath, composeDir)

	if first := Reconcile(t.Context(), Options{Trigger: "poll"}, deps); first.Err == nil {
		t.Fatal("expected the failed teardown to surface as an error")
	}
	if store.State.LastHealthyCommit != "aaa111" {
		t.Fatalf("LastHealthyCommit = %q, want the commit not promoted while a stack is still up", store.State.LastHealthyCommit)
	}

	compose.downErr = nil
	second := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if second.Err != nil {
		t.Fatalf("unexpected error on the retry: %v", second.Err)
	}
	if !slices.Contains(compose.downCalls[1:], "test-stack-b") {
		t.Errorf("downCalls = %v, want the orphaned stack torn down on the retry", compose.downCalls)
	}
	if store.State.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q once the teardown succeeded", store.State.LastHealthyCommit, "bbb222")
	}
}

func TestReconcileTransientSnapshotFailureDoesNotBurnARecoveryAttempt(t *testing.T) {
	st := withHealthy("aaa111")
	st.PendingCommit = "bbb222"
	snap := &fakeSnapshotter{err: errors.New("docker daemon unreachable")}
	deps, store := testDeps(t, gitNoChange("bbb222"), &fakeCompose{}, snap, st)

	if result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps); result.Err == nil {
		t.Fatal("expected the snapshot failure to surface")
	}
	if store.State.PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0: a Docker blip must not spend the recovery budget", store.State.PendingAttempts)
	}
}

func TestReconcileResumedRevertOnlyTouchesThePendingStacks(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "a", "docker-compose.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "b", "docker-compose.yaml"), validComposeYAML)

	st := withHealthy("aaa111")
	st.PendingCommit = "bbb222"
	st.LastFailedCommit = "bbb222"
	st.PendingStacks = []string{"test-stack-a"}

	compose := &fakeCompose{}
	deps, _ := testDirDeps(t, gitNoChange("bbb222"), compose, snapshots(healthySnapshot()), st, repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "poll"}, deps)

	if !result.Reverted {
		t.Fatalf("expected the revert to be resumed, result = %+v", result)
	}
	if len(compose.loadCalls) != 1 || compose.loadCalls[0].projectName != "test-stack-a" {
		t.Errorf("loadCalls = %+v, want only test-stack-a: an unrelated stack must not be re-upped", compose.loadCalls)
	}
	if len(compose.downCalls) != 0 {
		t.Errorf("downCalls = %v, want none", compose.downCalls)
	}
}

func TestReconcileFreshInstallAppliesEvenWhenTheCommitLooksIrrelevant(t *testing.T) {
	compose := &fakeCompose{}
	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "README.md"
	deps, store := testDeps(t, g, compose, snapshots(healthySnapshot()), state.New())

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if !result.Applied {
		t.Fatalf("expected the first deploy to run even for a docs-only commit, result = %+v", result)
	}
	if store.State.LastHealthyCommit != "bbb222" {
		t.Errorf("LastHealthyCommit = %q, want %q", store.State.LastHealthyCommit, "bbb222")
	}
}
