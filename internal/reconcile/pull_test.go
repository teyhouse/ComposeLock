package reconcile

import (
	"fmt"
	"path/filepath"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
)

func TestReconcileDirModeBadImageInOneStackTouchesNoStack(t *testing.T) {
	repoPath := t.TempDir()
	composeDir := filepath.Join(repoPath, "deployment")
	writeComposeFile(t, filepath.Join(composeDir, "service-a.yaml"), validComposeYAML)
	writeComposeFile(t, filepath.Join(composeDir, "db", "docker-compose.yaml"), validComposeYAML)

	g := gitChange("aaa111", "bbb222")
	g.diffFiles = "deployment/service-a.yaml\ndeployment/db/docker-compose.yaml"
	compose := &fakeCompose{pullErrs: map[string]error{
		"test-stack-db": fmt.Errorf("compose pull: %w", cerrdefs.ErrNotFound),
	}}
	deps, store := testDirDeps(t, g, compose, snapshots(healthySnapshot()), withHealthy("aaa111"), repoPath, composeDir)

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)

	if result.Err == nil || compose.upCalls != 0 {
		t.Fatalf("Err = %v, Up called %d times, want an error and no Up", result.Err, compose.upCalls)
	}
	if len(compose.pullCalls) != 2 {
		t.Errorf("pulled %v, want both stacks pulled before any Up", compose.pullCalls)
	}
	if !store.State.IsKnownBad("bbb222") || g.head != "aaa111" {
		t.Errorf("known bad = %v, HEAD = %q, want the commit marked bad and the checkout restored", store.State.IsKnownBad("bbb222"), g.head)
	}
}
