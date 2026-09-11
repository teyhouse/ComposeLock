package config

import (
	"errors"
	"fmt"
	"time"
)

type Overrides struct {
	RepoPath    *string
	Remote      *string
	Branch      *string
	ComposeFile *string
	ProjectName *string
	StateFile   *string
	SSHKey      *string
	Discord     *string
	LogFormat   *string

	RetryAttempts    *int
	RetryDelay       *time.Duration
	PollInterval     *time.Duration
	HealthWatch      *time.Duration
	HealthInterval   *time.Duration
	RestartTolerance *int
}

func (c *Config) Apply(o Overrides) error {
	setString(&c.RepoPath, o.RepoPath)
	setString(&c.Remote, o.Remote)
	setString(&c.Branch, o.Branch)
	setString(&c.ComposeFile, o.ComposeFile)
	setString(&c.ProjectName, o.ProjectName)
	setString(&c.StateFile, o.StateFile)
	setString(&c.SSHKey, o.SSHKey)
	setString(&c.DiscordWebhook, o.Discord)
	setString(&c.LogFormat, o.LogFormat)

	setInt(&c.RetryAttempts, o.RetryAttempts)
	setInt(&c.HealthRestartTolerance, o.RestartTolerance)

	return errors.Join(
		setSeconds(&c.RetryDelaySeconds, o.RetryDelay, "retry_delay_seconds"),
		setSeconds(&c.PollIntervalSeconds, o.PollInterval, "poll_interval_seconds"),
		setSeconds(&c.HealthWatchSeconds, o.HealthWatch, "health_watch_seconds"),
		setSeconds(&c.HealthPollIntervalSeconds, o.HealthInterval, "health_poll_interval_seconds"),
	)
}

func setString(dst *string, src *string) {
	if src != nil {
		*dst = *src
	}
}

func setInt(dst *int, src *int) {
	if src != nil {
		*dst = *src
	}
}

func setSeconds(dst *int, src *time.Duration, field string) error {
	if src == nil {
		return nil
	}
	if *src%time.Second != 0 {
		return fmt.Errorf("config: %s must be a whole number of seconds, got %s", field, *src)
	}
	*dst = int(*src / time.Second)
	return nil
}
