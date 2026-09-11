# ComposeLock

[![CI](https://github.com/teyhouse/ComposeLock/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/teyhouse/ComposeLock/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/teyhouse/ComposeLock)](https://github.com/teyhouse/ComposeLock/releases/latest)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

<img src="assets/logo.png" alt="ComposeLock logo" width="320">

ComposeLock is a small Go CLI that keeps a Docker Compose stack in sync
with a Git repository. It replaces manual `git pull && docker compose up`
or Portainer's GitOps stack sync with a version that will not apply a
change on top of a broken stack, and will not leave a broken change
running.

On each run it:

1. Fetches the configured Git branch and checks it against the local
   checkout.
2. Refuses to apply anything if the currently running stack is not
   healthy, or if the new commit already failed its health check once
   before.
3. Applies the new commit through the official Docker Compose SDK (no
   shelling out to `docker compose`).
4. Watches the result for a configurable window (default 5 minutes).
5. If the stack does not stay healthy for the full window, it reverts to
   the last commit that was known to be healthy and watches that too.

State (last healthy commit, last failed commit, pending commit) is kept
in a JSON file next to the binary so a crash mid-deploy is recoverable on
the next run.

Secrets are out of scope. ComposeLock will check that `env_file:` paths
referenced in the Compose file exist, but never opens, reads, or logs
them.

## Requirements

- Go 1.26 or newer to build.
- A reachable Docker daemon at runtime. The Compose SDK talks to it
  directly, so no `docker compose` CLI is required.
- `git` on `PATH`.

## Build

```sh
make build
```

Produces `bin/composelock`. Other targets:

```sh
make test     # go test -race -shuffle=on ./...
make lint     # go vet + staticcheck
make vuln     # govulncheck
make fmt      # gofmt
make install  # installs to /usr/local/bin
```

Cross-compile with `GOOS`/`GOARCH`, for example:

```sh
make build GOOS=linux GOARCH=amd64
```

## Configure

```sh
composelock --init --config composelock.json --with-state
```

This writes a default `composelock.json` and an empty `state.json`.
Config path resolution order: `--config` flag, then `$COMPOSELOCK_CONFIG`,
then `./composelock.json`.

Minimum fields to edit:

| Field           | Meaning                                    |
|-----------------|---------------------------------------------|
| `repo_path`     | Path to the local Git checkout               |
| `compose_file`  | Path to the `docker-compose.yml` to apply    |
| `project_name`  | Compose project name (required by the SDK)   |

Everything else has a sane default:

| Field                          | Default              | Meaning                                        |
|---------------------------------|-----------------------|-------------------------------------------------|
| `remote`                        | `origin`              | Git remote to fetch                             |
| `branch`                        | `main`                | Branch to track                                 |
| `ssh_key`                       | (system agent)        | SSH key for Git, if not using the agent         |
| `state_file`                    | `./state.json`        | Where reconcile state is stored                 |
| `retry_attempts`                | `3`                   | Git fetch retry attempts                        |
| `retry_delay_seconds`           | `20`                  | Delay between Git retries                       |
| `poll_interval_seconds`         | `0`                   | Used only by `composelock poll`; `0` disables it |
| `health_watch_seconds`          | `300`                 | How long to watch after apply/revert            |
| `health_poll_interval_seconds`  | `5`                   | Poll interval during the health watch           |
| `health_unhealthy_streak`       | `3`                   | Consecutive unhealthy polls before failing      |
| `health_restart_tolerance`      | `1`                   | Restarts allowed before treating it as a failure |
| `discord_webhook`               | (disabled)            | Discord webhook URL for notifications           |
| `log_format`                    | `json`                | `json` or `text`, logged to stdout              |
| `webhook.listen`                | `127.0.0.1:8080`      | Address for `composelock webhook`               |
| `webhook.path`                  | `/webhook`            | Path for `composelock webhook`                  |
| `webhook.secret`                | (disabled)            | HMAC secret for the webhook; empty disables validation |

Every field can be overridden on the command line, run
`composelock --help` for the full flag list. Flags win over the config
file.

## Commands

| Command    | Description                                                    |
|------------|------------------------------------------------------------------|
| `sync`     | One reconcile: fetch, gate, apply, watch, revert if needed       |
| `check`    | Same as `sync` but dry run: reports what would change, no writes |
| `status`   | Prints config path, state file contents, live container health  |
| `init`     | Scaffolds a default config (and state file with `--with-state`) |
| `poll`     | Runs `sync` in a loop every `poll_interval_seconds`              |
| `webhook`  | Starts an HTTP server that triggers `sync` on a Git push event  |
| `version`  | Prints version, commit, and build date                          |

Bare `composelock` with no command is equivalent to `sync`. `--version`
is also accepted as a flag and does the same as the `version` command.

### Exit codes

| Code | Meaning                                                       |
|------|-----------------------------------------------------------------|
| 0    | Success, nothing to do, or a known-bad commit was skipped     |
| 1    | Reconcile failed, but a revert brought the stack back healthy |
| 2    | Invalid usage or configuration                                |
| 3    | DEGRADED: revert also failed, manual intervention required    |

Once a run exits with code 3, every following run refuses to touch the
stack until it is retried with `--force`.

## Running as a cron job

`composelock sync` is a single, self-contained reconcile and is meant to
be invoked repeatedly, either by `composelock poll` as a long-running
process, or by cron. Use cron if you do not want a supervised background
process; use `poll` if you are already running this under systemd or a
similar supervisor.

A typical crontab entry, running every 5 minutes:

```
*/5 * * * * /usr/local/bin/composelock --config /etc/composelock/composelock.json sync >> /var/log/composelock.log 2>&1
```

Notes for cron specifically:

- Use absolute paths for the binary, `--config`, and every path inside
  the config file. Cron runs with a minimal environment and an
  unpredictable working directory.
- The user cron runs as needs permission to reach the Docker socket
  (usually membership in the `docker` group).
- `composelock` writes structured logs to stdout; redirecting them to a
  file, as above, is the simplest way to keep a record. `log_format` can
  be set to `text` if you prefer to read the log file directly instead of
  through a JSON log processor.
- Exit codes are meaningful for alerting. A wrapper script that checks
  `$?` after cron runs it, or cron's own `MAILTO`, is enough to get
  notified on exit codes 1 and 3. Discord notifications
  (`discord_webhook` in the config) cover the same cases without needing
  to parse cron output at all.
- Do not run `composelock poll` from cron. `poll` is a long-running loop
  and will simply pile up one process per cron tick.

## Versioning

ComposeLock follows [Semantic Versioning](https://semver.org). It is
currently `0.y.z`: the config format, CLI flags, and state file layout
may still change without a major bump.

The `VERSION` file at the repository root always holds the version that
will be used for the *next* release, for example `0.1.1`. Nobody needs to
touch it for routine changes: every release workflow run tags and
publishes exactly what is in `VERSION`, then increments the patch number
and commits `VERSION` back to `main` for next time. Patch releases are
therefore fully automatic.

Bump `VERSION` by hand only when a change is significant enough to
deserve a new major or minor line, a breaking config or CLI change, for
instance. Set it to whatever should be released next (`0.2.0`, `1.0.0`,
...) and commit it; the next relevant push releases exactly that version
and resumes auto-incrementing the patch from there.

Once the config format, CLI surface, and state file layout are
considered stable, bump `VERSION` to `1.0.0`.

## Continuous integration and releases

Every push to `main` that touches Go code (or `go.mod`/`go.sum`/the
`Makefile`/`VERSION`) runs `go vet`, `staticcheck`, the full test suite
under the race detector, and `govulncheck`. If those pass, a
`linux/amd64` binary is built, tagged with the version from `VERSION`
(see Versioning above), and published as a new GitHub Release. Pull
requests against `main` run the same checks without publishing anything.

See `.github/workflows/ci.yml`.

## License

See `LICENSE`.
