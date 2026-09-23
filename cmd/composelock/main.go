package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
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

func parseArgs(f *cliFlags, args []string) (string, error) {
	command, rest := "", args
	for {
		if err := f.fs.Parse(rest); err != nil {
			return "", err
		}
		if f.fs.NArg() == 0 {
			return command, nil
		}
		if command == "" {
			command = f.fs.Arg(0)
		}
		rest = f.fs.Args()[1:]
	}
}

func run(args []string) int {
	f := newCLIFlags()
	command, err := parseArgs(f, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if f.versionFlag {
		printVersion()
		return 0
	}

	if command == "" {
		if f.initFlag {
			command = "init"
		} else {
			command = "sync"
		}
	}

	switch command {
	case "version":
		printVersion()
		return 0
	case "init":
		return cmdInit(f)
	}

	if f.withState {
		fmt.Fprintf(os.Stderr, "warning: -with-state only applies to the init command, ignoring it for %s\n", command)
	}

	if err := checkFlagsFor(command, f); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	readOnly := command == "status" || command == "check" || (command == "sync" && f.dryRun)
	cfg, deps, code := setup(f, !readOnly)
	if code != 0 {
		return code
	}
	if closer, ok := deps.Compose.(io.Closer); ok {
		defer func() {
			if err := closer.Close(); err != nil {
				deps.Log.Warn("closing docker client", "err", err)
			}
		}()
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
	case "pause":
		return cmdPause(ctx, deps, f.pauseFor, f.pauseReason)
	case "resume":
		return cmdResume(ctx, deps)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		return 2
	}
}

func checkFlagsFor(command string, f *cliFlags) error {
	if command != "pause" {
		var misplaced []string
		f.fs.Visit(func(fl *flag.Flag) {
			if fl.Name == "for" || fl.Name == "reason" {
				misplaced = append(misplaced, "-"+fl.Name)
			}
		})
		if len(misplaced) > 0 {
			return fmt.Errorf("%s only applies to the pause command", strings.Join(misplaced, " and "))
		}
	}
	if command == "pause" && f.pauseFor < 0 {
		return fmt.Errorf("-for must not be negative, got %s", f.pauseFor)
	}
	if command != "poll" && command != "webhook" {
		return nil
	}
	var rejected []string
	f.fs.Visit(func(fl *flag.Flag) {
		if fl.Name != "dry-run" && fl.Name != "force" {
			return
		}
		if getter, ok := fl.Value.(flag.Getter); ok {
			if on, isBool := getter.Get().(bool); isBool && !on {
				return
			}
		}
		rejected = append(rejected, "-"+fl.Name)
	})
	if len(rejected) > 0 {
		return fmt.Errorf("%s does not accept %s: it always reconciles for real", command, strings.Join(rejected, " and "))
	}
	return nil
}
