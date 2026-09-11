package compose

import (
	"os"
	"path/filepath"
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
				EnvFiles: []types.EnvFile{{Path: "webapp.env"}},
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
				EnvFiles: []types.EnvFile{{Path: "webapp.env"}},
			},
		},
	}

	if err := CheckEnvFiles(project); err != nil {
		t.Fatalf("CheckEnvFiles: %v", err)
	}
}
