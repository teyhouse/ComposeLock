package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/teyhouse/ComposeLock/internal/compose"
	"github.com/teyhouse/ComposeLock/internal/config"
	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/reconcile"
	"github.com/teyhouse/ComposeLock/internal/state"
	"github.com/teyhouse/ComposeLock/internal/webhook"
)

const notifyDrainTimeout = 30 * time.Second

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
		"skip_reason", result.SkipReason,
		"applied", result.Applied,
		"reverted", result.Reverted,
		"degraded", result.Degraded,
		"restarted_services", result.Restarted,
		"old_commit", result.OldCommit,
		"new_commit", result.NewCommit,
		"updated_services", result.Updated,
		"duration_ms", result.Duration.Milliseconds(),
		"err", result.Err,
	)
}

func sendNotification(ctx context.Context, deps reconcile.Deps, result reconcile.Result) {
	if result.Notification != nil {
		deps.Notifier.SendThrottled(ctx, result.NotificationKey, *result.Notification)
	}
}

type asyncNotifier struct {
	deps reconcile.Deps
	wg   sync.WaitGroup
}

func (a *asyncNotifier) send(ctx context.Context, result reconcile.Result) {
	if result.Notification == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	a.wg.Go(func() { sendNotification(ctx, a.deps, result) })
}

func (a *asyncNotifier) drain() {
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(notifyDrainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		a.deps.Log.Warn("discord notify: pending notifications did not finish before exit")
	}
}

func cmdInit(f *cliFlags) int {
	path := config.ResolvePath(f.configPath)
	cfg, err := config.Init(path, f.force, f.overrides())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Println("wrote", path)

	if f.withState {
		statePath := cfg.StateFile
		if _, err := os.Stat(statePath); err == nil && !f.force {
			fmt.Fprintf(os.Stderr, "state file %s already exists (use --force to overwrite)\n", statePath)
			return 2
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err := state.Save(statePath, state.New()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Println("wrote", statePath)
	}
	return 0
}

func cmdStatus(ctx context.Context, configPath string, cfg *config.Config, deps reconcile.Deps) int {
	fmt.Println("config:", configPath)

	var st *state.State
	var stacks []compose.Stack
	var loadErr, discoverErr error
	if err := withStateLock(deps, func() {
		if st, loadErr = deps.State.Load(); loadErr != nil {
			return
		}
		stacks, discoverErr = reconcile.StacksFor(cfg, deps.Log)
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if loadErr != nil {
		fmt.Fprintln(os.Stderr, "reading state:", loadErr)
		return 2
	}
	printPauseAndHistory(st, time.Now())
	data, err := json.Marshal(st, jsontext.WithIndent("  "))
	if err != nil {
		fmt.Fprintln(os.Stderr, "encoding state:", err)
		return 1
	}
	fmt.Printf("state:\n%s\n", data)

	if discoverErr != nil {
		fmt.Fprintln(os.Stderr, "discovering compose stacks:", discoverErr)
		return 1
	}

	code := 0
	for _, s := range stacks {
		if c := printStackContainers(ctx, deps, s.ProjectName); c != 0 {
			code = c
		}
	}
	return code
}

func withStateLock(deps reconcile.Deps, fn func()) error {
	if deps.Lock == nil {
		fn()
		return nil
	}
	held, err := deps.Lock.TryLock()
	if err != nil {
		return fmt.Errorf("acquiring state lock: %w", err)
	}
	if held {
		defer func() {
			if err := deps.Lock.Unlock(); err != nil {
				deps.Log.Error("releasing state lock", "err", err)
			}
		}()
	} else {
		fmt.Println("note: a reconcile is in progress, state and containers may not agree")
	}
	fn()
	return nil
}

func printStackContainers(ctx context.Context, deps reconcile.Deps, projectName string) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := deps.Health.Snapshot(ctx, projectName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live health snapshot:", err)
		return 1
	}
	healthy, reason := health.Evaluate(snap)
	verdict := "HEALTHY"
	if !healthy {
		verdict = "UNHEALTHY: " + reason
	} else if len(snap.Containers) == 0 {
		verdict = "NO CONTAINERS"
	}
	fmt.Printf("live containers (%s): %s\n", projectName, verdict)
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
	sendNotification(context.WithoutCancel(ctx), deps, result)
	return exitCode(result)
}

func startMonitor(ctx context.Context, cfg *config.Config, deps reconcile.Deps) (stop func()) {
	if cfg.MonitorIntervalSeconds <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		reconcile.NewMonitor(deps).Run(ctx, time.Duration(cfg.MonitorIntervalSeconds)*time.Second)
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

func cmdPoll(ctx context.Context, cfg *config.Config, deps reconcile.Deps) int {
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		fmt.Fprintln(os.Stderr, "poll_interval_seconds must be > 0 to use the poll command")
		return 2
	}

	stopPprof := startPprof(ctx, cfg.PprofListen, deps.Log)
	defer stopPprof()

	stopMonitor := startMonitor(ctx, cfg, deps)
	defer stopMonitor()

	notifier := &asyncNotifier{deps: deps}
	defer notifier.drain()

	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		result := reconcile.Reconcile(ctx, reconcile.Options{Trigger: "poll"}, deps)
		logResult(deps.Log, "poll tick complete", result)
		notifier.send(ctx, result)

		if ctx.Err() != nil {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-timer.C:
		}
		timer.Reset(interval)
	}
}

func cmdWebhook(ctx context.Context, cfg *config.Config, deps reconcile.Deps) int {
	stopPprof := startPprof(ctx, cfg.PprofListen, deps.Log)
	defer stopPprof()

	stopMonitor := startMonitor(ctx, cfg, deps)
	defer stopMonitor()

	notifier := &asyncNotifier{deps: deps}
	defer notifier.drain()

	srv := webhook.New(webhook.Config{
		Addr:   cfg.Webhook.Listen,
		Path:   cfg.Webhook.Path,
		Secret: cfg.Webhook.Secret,
		Branch: cfg.Branch,
	}, func(rctx context.Context) {
		result := reconcile.Reconcile(rctx, reconcile.Options{Trigger: "webhook"}, deps)
		logResult(deps.Log, "webhook reconcile complete", result)
		notifier.send(rctx, result)
	}, deps.Log)

	deps.Log.Info("webhook server starting", "addr", cfg.Webhook.Listen, "path", cfg.Webhook.Path)
	if err := srv.ListenAndServe(ctx); err != nil {
		deps.Log.Error("webhook server exited", "err", err)
		return 1
	}
	return 0
}

func printPauseAndHistory(st *state.State, now time.Time) {
	if st.Paused(now) {
		fmt.Println("paused:", pauseDescription(st.PausedUntil, st.PauseReason))
	}
	if len(st.History) == 0 {
		return
	}
	fmt.Println("recent deployments (newest first):")
	for _, d := range slices.Backward(st.History) {
		line := fmt.Sprintf("  %s  %-17s %s", d.At.Local().Format(time.DateTime), d.Result, orNone(shortID(d.Commit)))
		if d.RolledBackTo != "" {
			line += " -> " + shortID(d.RolledBackTo)
		}
		if d.Repeats > 0 {
			line += fmt.Sprintf(" (x%d)", d.Repeats+1)
		}
		if d.Error != "" {
			line += "  " + d.Error
		}
		fmt.Println(line)
	}
}

func shortID(commit string) string {
	return commit[:min(len(commit), 7)]
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
