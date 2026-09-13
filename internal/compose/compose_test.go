package compose

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func TestProjectPathsCollectsEveryRuntimeInput(t *testing.T) {
	project := &types.Project{
		Name:         "app",
		WorkingDir:   "/repo/stack",
		ComposeFiles: []string{"/repo/stack/docker-compose.yml"},
		Services: types.Services{
			"web": types.ServiceConfig{
				Name:     "web",
				EnvFiles: []types.EnvFile{{Path: "web.env"}, {Path: "/etc/secrets/shared.env"}},
				Build:    &types.BuildConfig{Context: "src", Dockerfile: "src/Dockerfile"},
			},
			"remote": types.ServiceConfig{
				Name:  "remote",
				Build: &types.BuildConfig{Context: "https://github.com/example/repo.git"},
			},
		},
	}

	got := ProjectPaths(project)
	want := []string{
		"/etc/secrets/shared.env",
		"/repo/stack/.env",
		"/repo/stack/docker-compose.yml",
		"/repo/stack/src",
		"/repo/stack/src/Dockerfile",
		"/repo/stack/web.env",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ProjectPaths = %q, want %q", got, want)
	}
}

func TestProjectPathsSkipsRelativePathsWithoutAWorkingDir(t *testing.T) {
	project := &types.Project{
		Name:     "app",
		Services: types.Services{"web": types.ServiceConfig{Name: "web", EnvFiles: []types.EnvFile{{Path: "web.env"}}}},
	}
	if got := ProjectPaths(project); len(got) != 0 {
		t.Errorf("ProjectPaths = %q, want empty when the project has no working directory", got)
	}
}
