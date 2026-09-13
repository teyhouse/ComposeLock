package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
)

const schemaVersion = 1

const defaultPath = "composelock.json"

type WebhookConfig struct {
	Listen string `json:"listen"`
	Path   string `json:"path"`
	Secret string `json:"secret"`
}

type Config struct {
	SchemaVersion int `json:"schema_version"`

	RepoPath    string `json:"repo_path"`
	Remote      string `json:"remote"`
	Branch      string `json:"branch"`
	ComposeFile string `json:"compose_file"`
	ComposeDir  string `json:"compose_dir"`
	ProjectName string `json:"project_name"`
	SSHKey      string `json:"ssh_key"`

	StateFile string `json:"state_file"`

	RetryAttempts     int `json:"retry_attempts"`
	RetryDelaySeconds int `json:"retry_delay_seconds"`

	DockerTimeoutSeconds   int `json:"docker_timeout_seconds"`
	DockerUpTimeoutSeconds int `json:"docker_up_timeout_seconds"`

	PollIntervalSeconds int `json:"poll_interval_seconds"`

	HealthWatchSeconds        int `json:"health_watch_seconds"`
	HealthPollIntervalSeconds int `json:"health_poll_interval_seconds"`
	HealthUnhealthyStreak     int `json:"health_unhealthy_streak"`
	HealthRestartTolerance    int `json:"health_restart_tolerance"`

	DiscordWebhook string `json:"discord_webhook"`
	LogFormat      string `json:"log_format"`
	PprofListen    string `json:"pprof_listen"`

	Webhook WebhookConfig `json:"webhook"`
}

func Default() *Config {
	return &Config{
		SchemaVersion: schemaVersion,

		RepoPath:    ".",
		Remote:      "origin",
		Branch:      "main",
		ComposeFile: "./docker-compose.yml",
		ProjectName: "my-stack",
		SSHKey:      "",

		StateFile: "./state.json",

		RetryAttempts:     3,
		RetryDelaySeconds: 20,

		DockerTimeoutSeconds:   60,
		DockerUpTimeoutSeconds: 1800,

		PollIntervalSeconds: 0,

		HealthWatchSeconds:        300,
		HealthPollIntervalSeconds: 5,
		HealthUnhealthyStreak:     3,
		HealthRestartTolerance:    1,

		DiscordWebhook: "",
		LogFormat:      "json",
		PprofListen:    "",

		Webhook: WebhookConfig{
			Listen: "127.0.0.1:8080",
			Path:   "/webhook",
			Secret: "",
		},
	}
}

func loadDefaults() *Config {
	cfg := Default()
	cfg.RepoPath = ""
	cfg.ProjectName = ""
	cfg.ComposeFile = ""
	return cfg
}

func ResolvePath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("COMPOSELOCK_CONFIG"); env != "" {
		return env
	}
	return defaultPath
}

func Load(path string, overrides Overrides, logger *slog.Logger) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	warnUnknownFields(raw, reflect.TypeFor[Config](), "", logger)

	cfg := loadDefaults()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = schemaVersion
	}
	if cfg.SchemaVersion > schemaVersion {
		return nil, fmt.Errorf("config file %s has schema_version %d, this build understands %d: upgrade composelock", path, cfg.SchemaVersion, schemaVersion)
	}

	if err := cfg.Apply(overrides); err != nil {
		return nil, err
	}
	cfg.StateFile = canonicalPath(cfg.StateFile)

	composeFileExplicit := rawFieldString(raw, "compose_file") != "" || overrides.ComposeFile != nil
	if cfg.ComposeDir != "" && composeFileExplicit {
		logger.Warn("compose_dir is set: ignoring compose_file")
	}

	if err := cfg.Validate(logger); err != nil {
		return nil, err
	}
	return cfg, nil
}

func warnUnknownFields(raw map[string]json.RawMessage, t reflect.Type, prefix string, logger *slog.Logger) {
	known := knownFields(t)
	for key, value := range raw {
		field, ok := known[key]
		if !ok {
			logger.Warn("unknown config field", "field", prefix+key)
			continue
		}
		if field.Kind() != reflect.Struct {
			continue
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(value, &nested); err != nil {
			continue
		}
		warnUnknownFields(nested, field, prefix+key+".", logger)
	}
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	dir, base := filepath.Split(abs)
	resolved, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return abs
	}
	return filepath.Join(resolved, base)
}

func (c *Config) validateRequired() error {
	for _, f := range []struct {
		name  string
		value string
	}{
		{"repo_path", c.RepoPath},
		{"project_name", c.ProjectName},
		{"remote", c.Remote},
		{"branch", c.Branch},
		{"state_file", c.StateFile},
	} {
		if f.value == "" {
			return fmt.Errorf("config: %s is required", f.name)
		}
	}
	if c.ComposeFile == "" && c.ComposeDir == "" {
		return fmt.Errorf("config: one of compose_file or compose_dir is required")
	}
	if c.ProjectName != loader.NormalizeProjectName(c.ProjectName) {
		return fmt.Errorf("config: project_name %q must consist only of lowercase letters, digits, hyphens and underscores, and start with a letter or digit", c.ProjectName)
	}
	if !validGitRef(c.Remote) {
		return fmt.Errorf("config: remote %q must consist only of letters, digits, %q and must not start with %q or contain %q", c.Remote, "._/-", "-", "..")
	}
	if !validGitRef(c.Branch) {
		return fmt.Errorf("config: branch %q must consist only of letters, digits, %q and must not start with %q or contain %q", c.Branch, "._/-", "-", "..")
	}
	return c.validateComposePath()
}

func (c *Config) validateComposePath() error {
	field, path, wantDir := "compose_file", c.ComposeFile, false
	if c.ComposeDir != "" {
		field, path, wantDir = "compose_dir", c.ComposeDir, true
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.RepoPath, path)
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: checking %s %s: %w", field, path, err)
	}
	if info.IsDir() != wantDir {
		kind := "a file"
		if wantDir {
			kind = "a directory"
		}
		return fmt.Errorf("config: %s %s must be %s", field, path, kind)
	}
	return nil
}

func rawFieldString(raw map[string]json.RawMessage, field string) string {
	val, ok := raw[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(val, &s); err != nil {
		return ""
	}
	return s
}

func knownFields(t reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, t.NumField())
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			fields[name] = t.Field(i).Type
		}
	}
	return fields
}

func (c *Config) Validate(logger *slog.Logger) error {
	if err := c.validateRequired(); err != nil {
		return err
	}
	if c.HealthWatchSeconds < 0 {
		return fmt.Errorf("config: health_watch_seconds must be >= 0")
	}
	if c.HealthWatchSeconds == 0 {
		logger.Warn("health_watch_seconds is 0: health watch disabled, apply verification skipped")
	}
	if c.RetryAttempts < 1 {
		return fmt.Errorf("config: retry_attempts must be >= 1")
	}
	if c.RetryDelaySeconds < 0 {
		return fmt.Errorf("config: retry_delay_seconds must be >= 0")
	}
	if c.HealthRestartTolerance < 0 {
		return fmt.Errorf("config: health_restart_tolerance must be >= 0")
	}
	if c.HealthPollIntervalSeconds < 1 {
		return fmt.Errorf("config: health_poll_interval_seconds must be >= 1")
	}
	if c.HealthUnhealthyStreak < 1 {
		return fmt.Errorf("config: health_unhealthy_streak must be >= 1")
	}
	if c.PollIntervalSeconds < 0 {
		return fmt.Errorf("config: poll_interval_seconds must be >= 0")
	}
	if c.DockerTimeoutSeconds < 0 {
		return fmt.Errorf("config: docker_timeout_seconds must be >= 0")
	}
	if c.DockerUpTimeoutSeconds < 0 {
		return fmt.Errorf("config: docker_up_timeout_seconds must be >= 0")
	}
	if c.PprofListen != "" && !isLoopback(c.PprofListen) {
		return fmt.Errorf("config: pprof_listen must be a loopback address, got %q", c.PprofListen)
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return fmt.Errorf("config: log_format must be \"json\" or \"text\", got %q", c.LogFormat)
	}
	if err := c.Webhook.validate(); err != nil {
		return err
	}
	return nil
}

func (w WebhookConfig) validate() error {
	if !strings.HasPrefix(w.Path, "/") {
		return fmt.Errorf("config: webhook.path must start with %q, got %q", "/", w.Path)
	}
	if strings.ContainsAny(w.Path, " \t") {
		return fmt.Errorf("config: webhook.path must not contain whitespace, got %q", w.Path)
	}
	if _, _, err := net.SplitHostPort(w.Listen); err != nil {
		return fmt.Errorf("config: webhook.listen must be host:port, got %q", w.Listen)
	}
	if w.Secret == "" && !isLoopback(w.Listen) {
		return fmt.Errorf("config: webhook.secret is required when webhook.listen %q is not a loopback address: every request to webhook.path would trigger a deploy", w.Listen)
	}
	return nil
}

func validGitRef(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '/', r == '-':
		default:
			return false
		}
	}
	return true
}

func isLoopback(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func Init(path string, force bool, overrides Overrides) (*Config, error) {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("config file %s already exists (use --force to overwrite)", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("checking config file %s: %w", path, err)
		}
	}

	cfg := Default()
	if err := cfg.Apply(overrides); err != nil {
		return nil, err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling default config: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("writing config file %s: %w", path, err)
	}
	return cfg, nil
}
