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
	ComposeDir  *string
	ProjectName *string
	StateFile   *string
	SSHKey      *string
	Discord     *string
	LogFormat   *string
	PprofListen *string

	WebhookListen *string
	WebhookPath   *string

	RetryAttempts    *int
	RestartTolerance *int
	HealthStreak     *int

	RetryDelay     *time.Duration
	PollInterval   *time.Duration
	HealthWatch    *time.Duration
	HealthInterval *time.Duration
	DockerTimeout  *time.Duration
	DockerUp       *time.Duration
}

func (c *Config) Apply(o Overrides) error {
	setString(&c.RepoPath, o.RepoPath)
	setString(&c.Remote, o.Remote)
	setString(&c.Branch, o.Branch)
	setString(&c.ComposeFile, o.ComposeFile)
	setString(&c.ComposeDir, o.ComposeDir)
	setString(&c.ProjectName, o.ProjectName)
	setString(&c.StateFile, o.StateFile)
	setString(&c.SSHKey, o.SSHKey)
	setString(&c.DiscordWebhook, o.Discord)
	setString(&c.LogFormat, o.LogFormat)
	setString(&c.PprofListen, o.PprofListen)
	setString(&c.Webhook.Listen, o.WebhookListen)
	setString(&c.Webhook.Path, o.WebhookPath)

	setInt(&c.RetryAttempts, o.RetryAttempts)
	setInt(&c.HealthRestartTolerance, o.RestartTolerance)
	setInt(&c.HealthUnhealthyStreak, o.HealthStreak)

	return errors.Join(
		setSeconds(&c.RetryDelaySeconds, o.RetryDelay, "retry_delay_seconds"),
		setSeconds(&c.PollIntervalSeconds, o.PollInterval, "poll_interval_seconds"),
		setSeconds(&c.HealthWatchSeconds, o.HealthWatch, "health_watch_seconds"),
		setSeconds(&c.HealthPollIntervalSeconds, o.HealthInterval, "health_poll_interval_seconds"),
		setSeconds(&c.DockerTimeoutSeconds, o.DockerTimeout, "docker_timeout_seconds"),
		setSeconds(&c.DockerUpTimeoutSeconds, o.DockerUp, "docker_up_timeout_seconds"),
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
