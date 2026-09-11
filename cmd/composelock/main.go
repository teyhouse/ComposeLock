package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/teyhouse/ComposeLock/internal/config"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func printVersion() {
	fmt.Printf("composelock %s (commit %s, built %s)\n", version, commit, date)
}

func run(args []string) int {
	f := newCLIFlags()
	if err := f.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if f.versionFlag {
		printVersion()
		return 0
	}

	command := "sync"
	switch rest := f.fs.Args(); {
	case len(rest) > 0:
		command = rest[0]
	case f.initFlag:
		command = "init"
	}

	switch command {
	case "version":
		printVersion()
		return 0
	case "init":
		return cmdInit(f)
	}

	readOnly := command == "status" || command == "check" || (command == "sync" && f.dryRun)
	cfg, deps, code := setup(f, !readOnly)
	if code != 0 {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "sync":
		return cmdReconcile(ctx, "cli", f.dryRun, f.force, deps)
	case "check":
		return cmdReconcile(ctx, "cli", true, f.force, deps)
	case "status":
		return cmdStatus(ctx, config.ResolvePath(f.configPath), cfg, deps)
	case "poll":
		return cmdPoll(ctx, cfg, deps)
	case "webhook":
		return cmdWebhook(ctx, cfg, deps)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		return 2
	}
}
