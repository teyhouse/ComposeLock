package config

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRejectsWebhookPathThatWouldPanicServeMux(t *testing.T) {
	for _, path := range []string{"", "webhook", "hooks/deploy", "/with space"} {
		cfg := Default()
		cfg.Webhook.Path = path
		if err := cfg.Validate(slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("Validate() accepted webhook.path %q, want an error", path)
		}
	}
}

func TestAcceptedWebhookPathRegistersOnServeMux(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ServeMux panicked on a validated webhook.path: %v", r)
		}
	}()
	http.NewServeMux().HandleFunc("POST "+cfg.Webhook.Path, func(http.ResponseWriter, *http.Request) {})
}

func TestValidateRejectsBadWebhookListen(t *testing.T) {
	for _, listen := range []string{"", "127.0.0.1", "not a host:port:8080"} {
		cfg := Default()
		cfg.Webhook.Listen = listen
		if err := cfg.Validate(slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("Validate() accepted webhook.listen %q, want an error", listen)
		}
	}
}

func TestValidateRejectsUnknownLogFormat(t *testing.T) {
	cfg := Default()
	cfg.LogFormat = "yaml"
	err := cfg.Validate(slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "log_format") {
		t.Errorf("Validate() error = %v, want a log_format error", err)
	}
}

func TestValidateCatchesBlankedOverrides(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{"repo_path": ".", "compose_file": "x", "project_name": "y"}`))

	blank := ""
	if _, err := Load(path, Overrides{RepoPath: &blank}, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("expected an error when --repo blanks a required value")
	}
}

func TestValidateAcceptsRequiredValuesFromOverridesOnly(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{"compose_file": "x"}`))

	repo, name := ".", "my-stack"
	if _, err := Load(path, Overrides{RepoPath: &repo, ProjectName: &name}, testLogger(&bytes.Buffer{})); err != nil {
		t.Fatalf("Load: %v, want values supplied only on the command line to count", err)
	}
}

func TestValidateRejectsInvalidProjectName(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, []byte(`{"repo_path": ".", "compose_file": "x", "project_name": "MyStack"}`))

	if _, err := Load(path, Overrides{}, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("expected an error: compose rejects a project name that is not already normalized")
	}
}

func TestValidateRejectsComposeDirPointingAtAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deployment"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, dir, []byte(`{"repo_path": "`+dir+`", "compose_dir": "deployment", "project_name": "y"}`))

	if _, err := Load(path, Overrides{}, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("expected an error when compose_dir is a file")
	}
}

func TestValidateRejectsOptionLikeRemoteAndBranch(t *testing.T) {
	for _, tc := range []struct{ name, remote, branch string }{
		{"remote flag", "--upload-pack=/bin/sh", "main"},
		{"branch flag", "origin", "--upload-pack=/bin/sh"},
		{"branch traversal", "origin", "../../etc"},
		{"branch space", "origin", "main branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Remote, cfg.Branch = tc.remote, tc.branch
			if err := cfg.Validate(slog.New(slog.DiscardHandler)); err == nil {
				t.Errorf("Validate() = nil, want a rejection for remote %q branch %q", tc.remote, tc.branch)
			}
		})
	}
}

func TestValidateRequiresAWebhookSecretOffLoopback(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Listen = "0.0.0.0:8080"
	if err := cfg.Validate(slog.New(slog.DiscardHandler)); err == nil {
		t.Error("Validate() = nil, want an unauthenticated public webhook to be refused")
	}

	cfg.Webhook.Secret = "hunter2"
	if err := cfg.Validate(slog.New(slog.DiscardHandler)); err != nil {
		t.Errorf("Validate() = %v, want a secret-protected public webhook to be accepted", err)
	}
}

func TestValidateRejectsAPollIntervalLongerThanTheWatchWindow(t *testing.T) {
	cfg := Default()
	cfg.HealthWatchSeconds = 30
	cfg.HealthPollIntervalSeconds = 60
	if err := cfg.Validate(slog.New(slog.DiscardHandler)); err == nil {
		t.Error("Validate() = nil, want a poll interval longer than the watch window to be refused")
	}

	cfg.HealthPollIntervalSeconds = 30
	if err := cfg.Validate(slog.New(slog.DiscardHandler)); err != nil {
		t.Errorf("Validate() = %v, want a poll interval equal to the watch window to be accepted", err)
	}

	cfg.HealthWatchSeconds = 0
	cfg.HealthPollIntervalSeconds = 60
	if err := cfg.Validate(slog.New(slog.DiscardHandler)); err != nil {
		t.Errorf("Validate() = %v, want the check skipped when the watch is disabled", err)
	}
}

func TestLogLevelDefaultsToInfoAndRejectsUnknownLevels(t *testing.T) {
	path := writeConfig(t, t.TempDir(), []byte(`{"repo_path": ".", "compose_file": "x", "project_name": "y"}`))
	cfg, err := Load(path, Overrides{}, slog.New(slog.DiscardHandler))
	if err != nil || cfg.Level() != slog.LevelInfo {
		t.Fatalf("config without log_level: level %v, err %v, want info", cfg.Level(), err)
	}
	cfg.LogLevel = "verbose"
	if err := cfg.Validate(slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "log_level") {
		t.Errorf("Validate() error = %v, want a log_level error", err)
	}
}
