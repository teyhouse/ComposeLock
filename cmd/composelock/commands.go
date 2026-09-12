package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/reconcile"
	"github.com/teyhouse/ComposeLock/internal/state"
	"github.com/teyhouse/ComposeLock/internal/webhook"
)

func exitCode(result reconcile.Result) int {
	switch {
	case result.Degraded:
		return 3
	case result.Err != nil:
		return 1
	default:
		return 0
	}
}

func logResult(log *slog.Logger, msg string, result reconcile.Result) {
	log.Info(msg,
		"changed", result.Changed,
		"skipped", result.Skipped,
		"applied", result.Applied,
		"reverted", result.Reverted,
		"degraded", result.Degraded,
		"old_commit", result.OldCommit,
		"new_commit", result.NewCommit,
		"duration_ms", result.Duration.Milliseconds(),
		"err", result.Err,
	)
}

func cmdInit(f *cliFlags) int {
	path := config.ResolvePath(f.configPath)
	if err := config.Init(path, f.force); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Println("wrote", path)

	if f.withState {
		cfg := config.Default()
		if err := state.Save(cfg.StateFile, state.New()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Println("wrote", cfg.StateFile)
	}
	return 0
}

func cmdStatus(ctx context.Context, configPath string, cfg *config.Config, deps reconcile.Deps) int {
	fmt.Println("config:", configPath)

	st, err := deps.State.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading state:", err)
		return 2
	}
	data, err := json.Marshal(st, jsontext.WithIndent("  "))
	if err != nil {
		fmt.Fprintln(os.Stderr, "encoding state:", err)
		return 1
	}
	fmt.Printf("state:\n%s\n", data)

	stacks, err := reconcile.StacksFor(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "discovering compose stacks:", err)
		stacks = []compose.Stack{{ProjectName: cfg.ProjectName}}
	}

	for _, s := range stacks {
		if code := printStackContainers(ctx, deps, s.ProjectName); code != 0 {
			return code
		}
	}
	return 0
}

func printStackContainers(ctx context.Context, deps reconcile.Deps, projectName string) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := deps.Health.Snapshot(ctx, projectName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live health snapshot:", err)
		return 1
	}
	fmt.Printf("live containers (%s):\n", projectName)
	for _, c := range snap.Containers {
		fmt.Printf("  %s (%s): state=%s health=%s restarts=%d\n", c.Service, c.ID, c.State, c.Health, c.RestartCount)
	}
	return 0
}

func cmdReconcile(ctx context.Context, trigger string, dryRun, force bool, deps reconcile.Deps) int {
	result := reconcile.Reconcile(ctx, reconcile.Options{
		DryRun:  dryRun,
		Force:   force,
		Trigger: trigger,
	}, deps)

	logResult(deps.Log, "reconcile complete", result)
	return exitCode(result)
}

func cmdPoll(ctx context.Context, cfg *config.Config, deps reconcile.Deps) int {
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		fmt.Fprintln(os.Stderr, "poll_interval_seconds must be > 0 to use the poll command")
		return 2
	}

	stopPprof := startPprof(ctx, cfg.PprofListen, deps.Log)
	defer stopPprof()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		result := reconcile.Reconcile(ctx, reconcile.Options{Trigger: "poll"}, deps)
		logResult(deps.Log, "poll tick complete", result)

		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

func cmdWebhook(ctx context.Context, cfg *config.Config, deps reconcile.Deps) int {
	stopPprof := startPprof(ctx, cfg.PprofListen, deps.Log)
	defer stopPprof()

	srv := webhook.New(webhook.Config{
		Addr:   cfg.Webhook.Listen,
		Path:   cfg.Webhook.Path,
		Secret: cfg.Webhook.Secret,
	}, func(rctx context.Context) {
		result := reconcile.Reconcile(rctx, reconcile.Options{Trigger: "webhook"}, deps)
		logResult(deps.Log, "webhook reconcile complete", result)
	}, deps.Log)

	deps.Log.Info("webhook server starting", "addr", cfg.Webhook.Listen, "path", cfg.Webhook.Path)
	if err := srv.ListenAndServe(ctx); err != nil {
		deps.Log.Error("webhook server exited", "err", err)
		return 1
	}
	return 0
}
