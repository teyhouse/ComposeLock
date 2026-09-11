package config

import "time"

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

func (c *Config) Apply(o Overrides) {
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

	setSeconds(&c.RetryDelaySeconds, o.RetryDelay)
	setSeconds(&c.PollIntervalSeconds, o.PollInterval)
	setSeconds(&c.HealthWatchSeconds, o.HealthWatch)
	setSeconds(&c.HealthPollIntervalSeconds, o.HealthInterval)
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

func setSeconds(dst *int, src *time.Duration) {
	if src != nil {
		*dst = int(src.Seconds())
	}
}
