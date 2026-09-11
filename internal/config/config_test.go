package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func writeConfig(t *testing.T, dir string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, "composelock.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{
		"repo_path": ".",
		"compose_file": "./docker-compose.yml",
		"project_name": "my-stack"
	}`))

	var buf bytes.Buffer
	cfg, err := Load(path, testLogger(&buf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Remote != "origin" {
		t.Errorf("Remote = %q, want default %q", cfg.Remote, "origin")
	}
	if cfg.RetryAttempts != 3 {
		t.Errorf("RetryAttempts = %d, want default 3", cfg.RetryAttempts)
	}
	if cfg.HealthWatchSeconds != 300 {
		t.Errorf("HealthWatchSeconds = %d, want default 300", cfg.HealthWatchSeconds)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no warnings, got: %s", buf.String())
	}
}

func TestLoadUnknownFieldWarns(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{
		"repo_path": ".",
		"compose_file": "./docker-compose.yml",
		"project_name": "my-stack",
		"totally_unknown_field": true
	}`))

	var buf bytes.Buffer
	if _, err := Load(path, testLogger(&buf)); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !bytes.Contains(buf.Bytes(), []byte("totally_unknown_field")) {
		t.Errorf("expected WARN mentioning unknown field, got: %s", buf.String())
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"missing repo_path", `{"compose_file": "x", "project_name": "y"}`},
		{"missing compose_file", `{"repo_path": ".", "project_name": "y"}`},
		{"missing project_name", `{"repo_path": ".", "compose_file": "x"}`},
		{"negative retry_attempts", `{"repo_path": ".", "compose_file": "x", "project_name": "y", "retry_attempts": 0}`},
		{"negative retry_delay", `{"repo_path": ".", "compose_file": "x", "project_name": "y", "retry_delay_seconds": -1}`},
		{"negative restart tolerance", `{"repo_path": ".", "compose_file": "x", "project_name": "y", "health_restart_tolerance": -1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeConfig(t, dir, []byte(tt.json))
			var buf bytes.Buffer
			if _, err := Load(path, testLogger(&buf)); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestLoadZeroHealthWatchWarns(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{
		"repo_path": ".",
		"compose_file": "./docker-compose.yml",
		"project_name": "my-stack",
		"health_watch_seconds": 0
	}`))

	var buf bytes.Buffer
	if _, err := Load(path, testLogger(&buf)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("health_watch_seconds")) {
		t.Errorf("expected WARN about disabled health watch, got: %s", buf.String())
	}
}

func TestApplyOverrides(t *testing.T) {
	cfg := Default()

	branch := "develop"
	retryDelay := 45 * time.Second
	tolerance := 2

	cfg.Apply(Overrides{
		Branch:           &branch,
		RetryDelay:       &retryDelay,
		RestartTolerance: &tolerance,
	})

	if cfg.Branch != "develop" {
		t.Errorf("Branch = %q, want %q", cfg.Branch, "develop")
	}
	if cfg.RetryDelaySeconds != 45 {
		t.Errorf("RetryDelaySeconds = %d, want 45", cfg.RetryDelaySeconds)
	}
	if cfg.HealthRestartTolerance != 2 {
		t.Errorf("HealthRestartTolerance = %d, want 2", cfg.HealthRestartTolerance)
	}
	if cfg.Remote != "origin" {
		t.Errorf("Remote = %q, want unchanged default %q", cfg.Remote, "origin")
	}
}

func TestResolvePath(t *testing.T) {
	t.Setenv("COMPOSELOCK_CONFIG", "")
	if got := ResolvePath(""); got != defaultPath {
		t.Errorf("ResolvePath() = %q, want %q", got, defaultPath)
	}

	t.Setenv("COMPOSELOCK_CONFIG", "/env/config.json")
	if got := ResolvePath(""); got != "/env/config.json" {
		t.Errorf("ResolvePath() = %q, want env value", got)
	}

	if got := ResolvePath("/flag/config.json"); got != "/flag/config.json" {
		t.Errorf("ResolvePath() = %q, want flag value to win", got)
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "composelock.json")

	if err := Init(path, false); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	if err := Init(path, false); err == nil {
		t.Fatal("expected Init to refuse overwrite without --force")
	}
	if err := Init(path, true); err != nil {
		t.Fatalf("Init with force: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading scaffolded config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("scaffolded config is not valid JSON: %v", err)
	}
	if cfg.ProjectName != "my-stack" {
		t.Errorf("scaffolded ProjectName = %q, want %q", cfg.ProjectName, "my-stack")
	}
}
