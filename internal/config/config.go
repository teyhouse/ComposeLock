package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
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
	ProjectName string `json:"project_name"`
	SSHKey      string `json:"ssh_key"`

	StateFile string `json:"state_file"`

	RetryAttempts     int `json:"retry_attempts"`
	RetryDelaySeconds int `json:"retry_delay_seconds"`

	PollIntervalSeconds int `json:"poll_interval_seconds"`

	HealthWatchSeconds        int `json:"health_watch_seconds"`
	HealthPollIntervalSeconds int `json:"health_poll_interval_seconds"`
	HealthUnhealthyStreak     int `json:"health_unhealthy_streak"`
	HealthRestartTolerance    int `json:"health_restart_tolerance"`

	DiscordWebhook string `json:"discord_webhook"`
	LogFormat      string `json:"log_format"`

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

		PollIntervalSeconds: 0,

		HealthWatchSeconds:        300,
		HealthPollIntervalSeconds: 5,
		HealthUnhealthyStreak:     3,
		HealthRestartTolerance:    1,

		DiscordWebhook: "",
		LogFormat:      "json",

		Webhook: WebhookConfig{
			Listen: "127.0.0.1:8080",
			Path:   "/webhook",
			Secret: "",
		},
	}
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

func Load(path string, logger *slog.Logger) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	// Checked against the raw JSON, not the merged Config below: Default()
	// pre-fills repo_path/compose_file/project_name, so validating against
	// the merged struct would never catch an omitted key.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	warnUnknownFields(raw, logger)
	if err := validateRequired(raw); err != nil {
		return nil, err
	}

	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = schemaVersion
	}

	if err := cfg.Validate(logger); err != nil {
		return nil, err
	}
	return cfg, nil
}

func warnUnknownFields(raw map[string]json.RawMessage, logger *slog.Logger) {
	known := knownTopLevelFields()
	for key := range raw {
		if !known[key] {
			logger.Warn("unknown config field", "field", key)
		}
	}
}

func validateRequired(raw map[string]json.RawMessage) error {
	for _, field := range []string{"repo_path", "compose_file", "project_name"} {
		val, ok := raw[field]
		if !ok {
			return fmt.Errorf("config: %s is required", field)
		}
		var s string
		if err := json.Unmarshal(val, &s); err != nil || s == "" {
			return fmt.Errorf("config: %s is required", field)
		}
	}
	return nil
}

func knownTopLevelFields() map[string]bool {
	fields := make(map[string]bool)
	t := reflect.TypeFor[Config]()
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			fields[name] = true
		}
	}
	return fields
}

func (c *Config) Validate(logger *slog.Logger) error {
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
	return nil
}

func Init(path string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config file %s already exists (use --force to overwrite)", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking config file %s: %w", path, err)
		}
	}

	data, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling default config: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing config file %s: %w", path, err)
	}
	return nil
}
