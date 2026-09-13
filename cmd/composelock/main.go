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

func splitCommand(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func run(args []string) int {
	f := newCLIFlags()
	command, rest := splitCommand(args)
	if err := f.fs.Parse(rest); err != nil {
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
		switch {
		case f.fs.NArg() > 0:
			command = f.fs.Arg(0)
		case f.initFlag:
			command = "init"
		default:
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
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		return 2
	}
}

func checkFlagsFor(command string, f *cliFlags) error {
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
