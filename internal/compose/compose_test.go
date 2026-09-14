package compose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestCheckEnvFilesMissing(t *testing.T) {
	dir := t.TempDir()
	project := &types.Project{
		WorkingDir: dir,
		Services: types.Services{
			"webapp": types.ServiceConfig{
				Name:     "webapp",
				EnvFiles: []types.EnvFile{{Path: "webapp.env", Required: true}},
			},
		},
	}

	err := CheckEnvFiles(project)
	if err == nil {
		t.Fatal("expected error for missing env_file")
	}
	want := filepath.Join(dir, "webapp.env")
	got := err.Error()
	if !strings.Contains(got, want) || !strings.Contains(got, `"webapp"`) {
		t.Errorf("error = %q, want it to mention %q and service \"webapp\"", got, want)
	}
}

func TestCheckEnvFilesPresent(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "webapp.env")
	if err := os.WriteFile(envPath, []byte("FOO=bar\n"), 0o644); err != nil {
		t.Fatalf("writing env file: %v", err)
	}

	project := &types.Project{
		WorkingDir: dir,
		Services: types.Services{
			"webapp": types.ServiceConfig{
				Name:     "webapp",
				EnvFiles: []types.EnvFile{{Path: "webapp.env", Required: true}},
			},
		},
	}

	if err := CheckEnvFiles(project); err != nil {
		t.Fatalf("CheckEnvFiles: %v", err)
	}
}

func TestCheckEnvFilesIgnoresOptionalMissingFile(t *testing.T) {
	dir := t.TempDir()
	project := &types.Project{
		WorkingDir: dir,
		Services: types.Services{
			"webapp": types.ServiceConfig{
				Name:     "webapp",
				EnvFiles: []types.EnvFile{{Path: "optional.env", Required: false}},
			},
		},
	}

	if err := CheckEnvFiles(project); err != nil {
		t.Fatalf("CheckEnvFiles: %v, want no error for a missing env_file marked required: false", err)
	}
}

func TestCheckEnvFilesRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "webapp.env"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	project := &types.Project{
		WorkingDir: dir,
		Services: types.Services{
			"webapp": types.ServiceConfig{
				Name:     "webapp",
				EnvFiles: []types.EnvFile{{Path: "webapp.env", Required: true}},
			},
		},
	}

	err := CheckEnvFiles(project)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("err = %v, want a directory error", err)
	}
}

func TestProjectInputsCollectsEveryRuntimeInput(t *testing.T) {
	project := &types.Project{
		Name:       "app",
		WorkingDir: "/repo/stack",
		Services: types.Services{
			"web": types.ServiceConfig{
				Name:     "web",
				EnvFiles: []types.EnvFile{{Path: "web.env"}, {Path: "/etc/secrets/shared.env"}},
				// dockerfile is relative to the build context, which is what the
				// loader leaves in this field after resolving context itself.
				Build: &types.BuildConfig{Context: "src", Dockerfile: "Dockerfile"},
			},
			"remote": types.ServiceConfig{
				Name:  "remote",
				Build: &types.BuildConfig{Context: "https://github.com/example/repo.git"},
			},
		},
	}

	got, complete := ProjectInputs(project)
	want := []string{
		"/etc/secrets/shared.env",
		"/repo/stack/.env",
		"/repo/stack/src",
		"/repo/stack/src/Dockerfile",
		"/repo/stack/web.env",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ProjectInputs = %q, want %q", got, want)
	}
	if !complete {
		t.Error("expected the input set to be complete")
	}
}

func TestProjectInputsSkipsRelativePathsWithoutAWorkingDir(t *testing.T) {
	project := &types.Project{
		Name:     "app",
		Services: types.Services{"web": types.ServiceConfig{Name: "web", EnvFiles: []types.EnvFile{{Path: "web.env"}}}},
	}
	if got, _ := ProjectInputs(project); len(got) != 0 {
		t.Errorf("ProjectInputs = %q, want empty when the project has no working directory", got)
	}
}

func TestForceBuildTargetsOnlyBuildableServices(t *testing.T) {
	project := &types.Project{
		Services: types.Services{
			"web": types.ServiceConfig{Name: "web", Build: &types.BuildConfig{Context: "."}},
			"db":  types.ServiceConfig{Name: "db", Image: "postgres:16"},
		},
	}

	forceBuild(project)

	if got := project.Services["web"].PullPolicy; got != types.PullPolicyBuild {
		t.Errorf("web PullPolicy = %q, want %q: a build: service must be rebuilt even when a previous image of the same tag is still local",
			got, types.PullPolicyBuild)
	}
	if got := project.Services["db"].PullPolicy; got != "" {
		t.Errorf("db PullPolicy = %q, want it untouched: an image-only service has nothing to build", got)
	}
}

func TestOwnDeadlineIsNotAnInfrastructureError(t *testing.T) {
	caller := context.Background()
	inner, cancel := context.WithDeadline(caller, time.Now().Add(-time.Second))
	defer cancel()

	err := ownDeadline(caller, inner, context.DeadlineExceeded, 30*time.Minute)

	if IsInfraError(err) {
		t.Errorf("our own docker_up_timeout_seconds expiring must not read as the environment failing, or the apply is left pending and the half-converged stack is never reverted")
	}
	if !strings.Contains(err.Error(), "30m0s") {
		t.Errorf("err = %v, want it to name the budget that ran out", err)
	}
}

func TestOwnDeadlineKeepsCallerCancellation(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	inner, cancel := context.WithCancel(caller)
	defer cancel()

	err := ownDeadline(caller, inner, context.Canceled, time.Minute)

	if !IsInfraError(err) {
		t.Errorf("a shutdown is still the environment, not the commit: err = %v", err)
	}
}

func TestOwnDeadlineLeavesOtherErrorsAlone(t *testing.T) {
	caller := context.Background()
	inner, cancel := context.WithCancel(caller)
	defer cancel()
	original := errors.New("image pull failed")

	if err := ownDeadline(caller, inner, original, time.Minute); !errors.Is(err, original) {
		t.Errorf("err = %v, want the original error untouched", err)
	}
}

func TestDiscoverDoesNotCacheFailures(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"my db", "my-db"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "compose.yaml"), []byte(validComposeYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Discover(dir, "proj", nil); err == nil {
		t.Fatal("expected colliding project names to fail discovery")
	}

	cache.mu.Lock()
	cachedErr, cachedFingerprint := cache.err, cache.fingerprint
	cache.mu.Unlock()

	if cachedErr != nil {
		t.Errorf("cache holds err = %v: a failure cached against a still-valid fingerprint keeps discovery broken after the cause clears", cachedErr)
	}
	if cachedFingerprint != "" && cachedErr != nil {
		t.Errorf("a failed discovery must not claim a valid fingerprint")
	}
}
