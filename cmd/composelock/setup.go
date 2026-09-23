package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/gofrs/flock"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/execx"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/heartbeat"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/reconcile"
	"github.com/teyhouse/ComposeLock/internal/state"
)

func newLogger(format string, out io.Writer) *slog.Logger {
	if format == "text" {
		return slog.New(slog.NewTextHandler(out, nil))
	}
	return slog.New(slog.NewJSONHandler(out, nil))
}

func loadConfig(f *cliFlags, bootLogger *slog.Logger) (*config.Config, error) {
	cfg, err := config.Load(config.ResolvePath(f.configPath), f.overrides(), bootLogger)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}

func buildDeps(cfg *config.Config, log *slog.Logger) (reconcile.Deps, error) {
	dockerTimeout := time.Duration(cfg.DockerTimeoutSeconds) * time.Second
	composeSvc, err := compose.New(dockerTimeout, time.Duration(cfg.DockerUpTimeoutSeconds)*time.Second)
	if err != nil {
		return reconcile.Deps{}, fmt.Errorf("initializing compose service: %w", err)
	}

	return reconcile.Deps{
		Config: cfg,
		Git: &git.Syncer{
			Runner:   execx.OSRunner{},
			RepoPath: cfg.RepoPath,
			Remote:   cfg.Remote,
			Branch:   cfg.Branch,
			SSHKey:   cfg.SSHKey,
		},
		Compose:   composeSvc,
		Health:    &health.ComposeSnapshotter{Service: composeSvc, Timeout: dockerTimeout},
		Clock:     health.RealClock{},
		State:     state.FileStore{Path: cfg.StateFile},
		Notifier:  notify.New(cfg.DiscordWebhook, log),
		Heartbeat: heartbeat.New(cfg.HeartbeatURL, log),
		Log:       log,
	}, nil
}

func setup(f *cliFlags, writesState bool) (*config.Config, reconcile.Deps, int) {
	bootLogger := newLogger("json", os.Stdout)

	cfg, err := loadConfig(f, bootLogger)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, reconcile.Deps{}, 2
	}

	log := newLogger(cfg.LogFormat, os.Stdout)
	log.Info("composelock starting", "version", version, "commit", commit, "built", date)

	deps, err := buildDeps(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, reconcile.Deps{}, 2
	}

	if _, err := deps.State.Load(); err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("reading state: %w", err))
		return nil, reconcile.Deps{}, 2
	}
	if writesState {
		if err := state.CheckWritable(cfg.StateFile); err != nil {
			fmt.Fprintln(os.Stderr, fmt.Errorf("state file unwritable: %w", err))
			return nil, reconcile.Deps{}, 2
		}
		deps.Lock = flock.New(cfg.StateFile + ".lock")
	} else {
		deps.Lock = sharedLock{flock.New(cfg.StateFile + ".lock")}
	}

	return cfg, deps, 0
}

type sharedLock struct {
	*flock.Flock
}

func (l sharedLock) TryLock() (bool, error) { return l.Flock.TryRLock() }
