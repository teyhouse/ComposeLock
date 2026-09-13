package main

import (
	"flag"
	"time"

	"github.com/teyhouse/ComposeLock/internal/config"
)

type cliFlags struct {
	fs *flag.FlagSet

	configPath  string
	repo        string
	remote      string
	branch      string
	composeFile string
	composeDir  string
	projectName string
	stateFile   string
	discord     string
	sshKey      string
	logFormat   string
	pprofListen string

	webhookListen string
	webhookPath   string

	retryAttempts    int
	restartTolerance int
	healthStreak     int

	retryDelay     time.Duration
	pollInterval   time.Duration
	healthWatch    time.Duration
	healthInterval time.Duration
	dockerTimeout  time.Duration
	dockerUp       time.Duration

	dryRun      bool
	force       bool
	initFlag    bool
	withState   bool
	versionFlag bool
}

func newCLIFlags() *cliFlags {
	f := &cliFlags{fs: flag.NewFlagSet("composelock", flag.ContinueOnError)}

	f.fs.StringVar(&f.configPath, "config", "", "Path to config JSON (default: $COMPOSELOCK_CONFIG or ./composelock.json)")
	f.fs.StringVar(&f.repo, "repo", "", "Path to local Git repo")
	f.fs.StringVar(&f.remote, "remote", "", "Git remote name")
	f.fs.StringVar(&f.branch, "branch", "", "Branch to track")
	f.fs.StringVar(&f.composeFile, "compose-file", "", "Path to docker-compose.yml")
	f.fs.StringVar(&f.composeDir, "compose-dir", "", "Directory of compose files (see README); overrides -compose-file")
	f.fs.StringVar(&f.projectName, "project-name", "", "Compose project name (required for SDK)")
	f.fs.StringVar(&f.stateFile, "state-file", "", "Path to state JSON")
	f.fs.StringVar(&f.discord, "discord", "", "Discord webhook URL")
	f.fs.StringVar(&f.sshKey, "ssh-key", "", "Path to SSH key for Git")
	f.fs.IntVar(&f.retryAttempts, "retry-attempts", 0, "Git sync retry attempts (default 3)")
	f.fs.DurationVar(&f.retryDelay, "retry-delay", 0, "Delay between Git retries (default 20s)")
	f.fs.DurationVar(&f.pollInterval, "poll-interval", 0, "Poll interval for `poll` (default 5m)")
	f.fs.DurationVar(&f.healthWatch, "health-watch", 0, "Health watch window (default 5m)")
	f.fs.DurationVar(&f.healthInterval, "health-interval", 0, "Health poll interval (default 5s)")
	f.fs.IntVar(&f.restartTolerance, "restart-tolerance", 0, "Restarts allowed before failing (default 1)")
	f.fs.IntVar(&f.healthStreak, "health-streak", 0, "Consecutive unhealthy polls before failing (default 3)")
	f.fs.DurationVar(&f.dockerTimeout, "docker-timeout", 0, "Timeout for Docker calls other than up (default 60s)")
	f.fs.DurationVar(&f.dockerUp, "docker-up-timeout", 0, "Timeout for compose up, which may pull or build (default 30m)")
	f.fs.StringVar(&f.pprofListen, "pprof-listen", "", "Loopback address for pprof in poll/webhook mode")
	f.fs.StringVar(&f.webhookListen, "webhook-listen", "", "Address for `webhook` (default 127.0.0.1:8080)")
	f.fs.StringVar(&f.webhookPath, "webhook-path", "", "Path for `webhook` (default /webhook)")
	f.fs.StringVar(&f.logFormat, "log-format", "", "json|text (default json)")
	f.fs.BoolVar(&f.dryRun, "dry-run", false, "Equivalent to `check`")
	f.fs.BoolVar(&f.force, "force", false, "Override DEGRADED and the known-bad-commit skip; apply anyway")
	f.fs.BoolVar(&f.initFlag, "init", false, "Scaffold a default config file")
	f.fs.BoolVar(&f.withState, "with-state", false, "With --init, also create an empty state file")
	f.fs.BoolVar(&f.versionFlag, "version", false, "Print version and exit")

	return f
}

func (f *cliFlags) overrides() config.Overrides {
	var o config.Overrides
	f.fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "repo":
			o.RepoPath = &f.repo
		case "remote":
			o.Remote = &f.remote
		case "branch":
			o.Branch = &f.branch
		case "compose-file":
			o.ComposeFile = &f.composeFile
		case "compose-dir":
			o.ComposeDir = &f.composeDir
		case "project-name":
			o.ProjectName = &f.projectName
		case "state-file":
			o.StateFile = &f.stateFile
		case "ssh-key":
			o.SSHKey = &f.sshKey
		case "discord":
			o.Discord = &f.discord
		case "log-format":
			o.LogFormat = &f.logFormat
		case "pprof-listen":
			o.PprofListen = &f.pprofListen
		case "webhook-listen":
			o.WebhookListen = &f.webhookListen
		case "webhook-path":
			o.WebhookPath = &f.webhookPath
		case "retry-attempts":
			o.RetryAttempts = &f.retryAttempts
		case "retry-delay":
			o.RetryDelay = &f.retryDelay
		case "poll-interval":
			o.PollInterval = &f.pollInterval
		case "health-watch":
			o.HealthWatch = &f.healthWatch
		case "health-interval":
			o.HealthInterval = &f.healthInterval
		case "health-streak":
			o.HealthStreak = &f.healthStreak
		case "restart-tolerance":
			o.RestartTolerance = &f.restartTolerance
		case "docker-timeout":
			o.DockerTimeout = &f.dockerTimeout
		case "docker-up-timeout":
			o.DockerUp = &f.dockerUp
		}
	})
	return o
}
