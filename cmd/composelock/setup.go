package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/execx"
	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/health"
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

func loadConfig(f *cliFlags, bootLogger *slog.Logger) (*config.Config, string, error) {
	path := config.ResolvePath(f.configPath)
	cfg, err := config.Load(path, bootLogger)
	if err != nil {
		return nil, path, fmt.Errorf("loading config: %w", err)
	}
	cfg.Apply(f.overrides())
	return cfg, path, nil
}

func buildDeps(cfg *config.Config, log *slog.Logger) (reconcile.Deps, error) {
	composeSvc, err := compose.New()
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
		Compose:  composeSvc,
		Health:   &health.ComposeSnapshotter{Service: composeSvc},
		Clock:    health.RealClock{},
		State:    state.FileStore{Path: cfg.StateFile},
		Notifier: notify.New(cfg.DiscordWebhook, log),
		Log:      log,
	}, nil
}

func setup(f *cliFlags) (*config.Config, reconcile.Deps, int) {
	bootLogger := newLogger("json", os.Stdout)

	cfg, _, err := loadConfig(f, bootLogger)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, reconcile.Deps{}, 2
	}

	log := newLogger(cfg.LogFormat, os.Stdout)

	deps, err := buildDeps(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, reconcile.Deps{}, 2
	}

	if _, err := deps.State.Load(); err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("state file unwritable: %w", err))
		return nil, reconcile.Deps{}, 2
	}

	return cfg, deps, 0
}
