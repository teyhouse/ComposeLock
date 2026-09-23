package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/teyhouse/ComposeLock/internal/reconcile"
	"github.com/teyhouse/ComposeLock/internal/state"
)

const lockRetryInterval = time.Second

func cmdPause(ctx context.Context, deps reconcile.Deps, pauseFor time.Duration, reason string) int {
	now := deps.Clock.Now()
	var until time.Time
	if pauseFor > 0 {
		until = now.Add(pauseFor)
	}
	err := updateState(ctx, deps, func(st *state.State) {
		st.Pause(now, until, reason)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	attrs := []any{"reason", reason}
	if !until.IsZero() {
		attrs = append(attrs, "until", until)
	}
	deps.Log.Info("deployments paused", attrs...)
	fmt.Println("deployments paused " + pauseDescription(until, reason))
	return 0
}

func cmdResume(ctx context.Context, deps reconcile.Deps) int {
	wasPaused := false
	err := updateState(ctx, deps, func(st *state.State) {
		wasPaused = st.Paused(deps.Clock.Now())
		st.Resume()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !wasPaused {
		fmt.Println("deployments were not paused")
		return 0
	}
	deps.Log.Info("deployments resumed")
	fmt.Println("deployments resumed")
	return 0
}

func updateState(ctx context.Context, deps reconcile.Deps, change func(*state.State)) error {
	if err := waitForLock(ctx, deps); err != nil {
		return err
	}
	defer func() {
		if err := deps.Lock.Unlock(); err != nil {
			deps.Log.Error("releasing state lock", "err", err)
		}
	}()
	st, err := deps.State.Load()
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}
	change(st)
	if err := deps.State.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}

func waitForLock(ctx context.Context, deps reconcile.Deps) error {
	waiting := false
	for {
		held, err := deps.Lock.TryLock()
		if err != nil {
			return fmt.Errorf("acquiring state lock: %w", err)
		}
		if held {
			return nil
		}
		if !waiting {
			fmt.Println("waiting for the running reconcile to finish (Ctrl-C to abort)")
			waiting = true
		}
		timer := time.NewTimer(lockRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("gave up waiting for the state lock: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func pauseDescription(until time.Time, reason string) string {
	out := "until resumed"
	if !until.IsZero() {
		out = "until " + until.Local().Format(time.DateTime)
	}
	if reason != "" {
		out += " (" + reason + ")"
	}
	return out
}
