package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	cfg, err := Load(path, Overrides{}, testLogger(&buf))
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

func TestLoadAbsolutisesRepoPath(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{
		"repo_path": ".",
		"compose_file": "./docker-compose.yml",
		"project_name": "my-stack"
	}`))

	cfg, err := Load(path, Overrides{}, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !filepath.IsAbs(cfg.RepoPath) {
		t.Fatalf("RepoPath = %q, want an absolute path: the reconciler compares paths built from it against the absolute paths compose reports for a project's inputs", cfg.RepoPath)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoPath != wd {
		t.Errorf("RepoPath = %q, want %q", cfg.RepoPath, wd)
	}
}

func TestLoadAbsolutisesRepoPathFromAFlag(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{
		"repo_path": "/srv/ignored",
		"compose_file": "./docker-compose.yml",
		"project_name": "my-stack"
	}`))

	repo := "./sub"
	cfg, err := Load(path, Overrides{RepoPath: &repo}, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !filepath.IsAbs(cfg.RepoPath) {
		t.Errorf("RepoPath = %q, want a relative --repo to be absolutised too", cfg.RepoPath)
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
	if _, err := Load(path, Overrides{}, testLogger(&buf)); err != nil {
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
		{"zero health poll interval", `{"repo_path": ".", "compose_file": "x", "project_name": "y", "health_poll_interval_seconds": 0}`},
		{"zero unhealthy streak", `{"repo_path": ".", "compose_file": "x", "project_name": "y", "health_unhealthy_streak": 0}`},
		{"negative poll interval", `{"repo_path": ".", "compose_file": "x", "project_name": "y", "poll_interval_seconds": -1}`},
		{"public pprof listener", `{"repo_path": ".", "compose_file": "x", "project_name": "y", "pprof_listen": "0.0.0.0:6060"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeConfig(t, dir, []byte(tt.json))
			var buf bytes.Buffer
			if _, err := Load(path, Overrides{}, testLogger(&buf)); err == nil {
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
	if _, err := Load(path, Overrides{}, testLogger(&buf)); err != nil {
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

	if err := cfg.Apply(Overrides{
		Branch:           &branch,
		RetryDelay:       &retryDelay,
		RestartTolerance: &tolerance,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

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

func TestApplyRejectsSubSecondDurations(t *testing.T) {
	cfg := Default()
	half := 500 * time.Millisecond

	if err := cfg.Apply(Overrides{HealthInterval: &half}); err == nil {
		t.Fatal("expected an error for a sub-second duration")
	}
	if cfg.HealthPollIntervalSeconds != 5 {
		t.Errorf("HealthPollIntervalSeconds = %d, want default 5 kept", cfg.HealthPollIntervalSeconds)
	}
}

func TestLoadValidatesOverrides(t *testing.T) {
	path := writeConfig(t, t.TempDir(), []byte(`{
		"repo_path": ".",
		"compose_file": "./docker-compose.yml",
		"project_name": "my-stack"
	}`))

	zero := 0
	negative := -time.Minute
	tests := []struct {
		name      string
		overrides Overrides
	}{
		{"zero retry attempts", Overrides{RetryAttempts: &zero}},
		{"negative health watch", Overrides{HealthWatch: &negative}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := Load(path, tt.overrides, testLogger(&buf)); err == nil {
				t.Fatal("expected validation error for override, got nil")
			}
		})
	}
}

func TestLoadAcceptsLoopbackPprofListener(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:6060", "localhost:6060", "[::1]:6060"} {
		t.Run(addr, func(t *testing.T) {
			path := writeConfig(t, t.TempDir(), []byte(`{
				"repo_path": ".",
				"compose_file": "./docker-compose.yml",
				"project_name": "my-stack",
				"pprof_listen": "`+addr+`"
			}`))
			var buf bytes.Buffer
			if _, err := Load(path, Overrides{}, testLogger(&buf)); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
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

	if _, err := Init(path, false, Overrides{}); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	if _, err := Init(path, false, Overrides{}); err == nil {
		t.Fatal("expected Init to refuse overwrite without --force")
	}
	if _, err := Init(path, true, Overrides{}); err != nil {
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

func TestInitWritesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "composelock.json")
	if _, err := Init(path, false, Overrides{}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 600", perm)
	}
}

func TestLoadComposeDirAloneIsSufficient(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{
		"repo_path": ".",
		"compose_dir": "./deployment",
		"project_name": "my-stack"
	}`))

	var buf bytes.Buffer
	cfg, err := Load(path, Overrides{}, testLogger(&buf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ComposeDir != "./deployment" {
		t.Errorf("ComposeDir = %q, want %q", cfg.ComposeDir, "./deployment")
	}
	if bytes.Contains(buf.Bytes(), []byte("ignoring compose_file")) {
		t.Errorf("expected no warning when compose_file was never set, got: %s", buf.String())
	}
}

func TestLoadComposeDirAndComposeFileBothSetWarns(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{
		"repo_path": ".",
		"compose_file": "./docker-compose.yml",
		"compose_dir": "./deployment",
		"project_name": "my-stack"
	}`))

	var buf bytes.Buffer
	cfg, err := Load(path, Overrides{}, testLogger(&buf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ComposeDir != "./deployment" {
		t.Errorf("ComposeDir = %q, want %q", cfg.ComposeDir, "./deployment")
	}
	if !bytes.Contains(buf.Bytes(), []byte("ignoring compose_file")) {
		t.Errorf("expected a warning that compose_file is ignored, got: %s", buf.String())
	}
}

func TestLoadComposeDirViaOverrideDoesNotWarnWithoutExplicitComposeFile(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{
		"repo_path": ".",
		"compose_dir": "./deployment",
		"project_name": "my-stack"
	}`))

	var buf bytes.Buffer
	if _, err := Load(path, Overrides{}, testLogger(&buf)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("ignoring compose_file")) {
		t.Errorf("expected no warning, got: %s", buf.String())
	}
}

func TestLoadMissingBothComposeFileAndComposeDirErrors(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{"repo_path": ".", "project_name": "y"}`))

	if _, err := Load(path, Overrides{}, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("expected an error when neither compose_file nor compose_dir is set")
	}
}

func TestLoadRejectsAFutureSchemaVersion(t *testing.T) {
	path := writeConfig(t, t.TempDir(), []byte(`{"schema_version": 99, "repo_path": ".", "project_name": "app", "compose_file": "./docker-compose.yml"}`))

	if _, err := Load(path, Overrides{}, testLogger(&bytes.Buffer{})); err == nil {
		t.Error("Load() = nil, want a config written by a newer build to be refused")
	}
}

func TestLoadWarnsAboutUnknownNestedFields(t *testing.T) {
	path := writeConfig(t, t.TempDir(), []byte(`{"repo_path": ".", "project_name": "app", "compose_file": "./docker-compose.yml", "webhook": {"secrt": "hunter2"}}`))

	var buf bytes.Buffer
	if _, err := Load(path, Overrides{}, testLogger(&buf)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(buf.String(), "webhook.secrt") {
		t.Errorf("logs = %q, want a warning naming webhook.secrt", buf.String())
	}
}

func TestLoadCanonicalizesTheStateFilePath(t *testing.T) {
	path := writeConfig(t, t.TempDir(), []byte(`{"repo_path": ".", "project_name": "app", "compose_file": "./docker-compose.yml", "state_file": "./sub/../state.json"}`))

	cfg, err := Load(path, Overrides{}, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !filepath.IsAbs(cfg.StateFile) || strings.Contains(cfg.StateFile, "..") {
		t.Errorf("StateFile = %q, want a cleaned absolute path", cfg.StateFile)
	}
}

func TestInitWritesTheStateFilePathItWasGiven(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "composelock.json")
	statePath := filepath.Join(dir, "var", "state.json")

	cfg, err := Init(path, false, Overrides{StateFile: &statePath})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if cfg.StateFile != statePath {
		t.Errorf("cfg.StateFile = %q, want %q", cfg.StateFile, statePath)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), statePath) {
		t.Errorf("scaffolded config does not point at %q:\n%s", statePath, data)
	}
}
