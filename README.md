<p align="center">
  <img src="assets/logo.png" alt="ComposeLock logo" width="320">
</p>

# ComposeLock

[![CI](https://github.com/teyhouse/ComposeLock/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/teyhouse/ComposeLock/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/teyhouse/ComposeLock)](https://github.com/teyhouse/ComposeLock/releases/latest)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)
[![LLM](https://img.shields.io/badge/LLM-Used_in_Development-9cf)](#ai-development-disclosure)

ComposeLock is a small Go CLI that keeps a Docker Compose stack in sync with a Git repository. It replaces manual `git pull && docker compose up` or Portainer's GitOps stack sync with a version that will not apply a change on top of a broken stack, and will not leave a broken change running.

On each run it:

1. Fetches the configured Git branch and checks it against the local checkout.
2. Refuses to apply anything if the currently running stack is not healthy, or if the new commit already failed its health check once before.
3. Applies the new commit through the official Docker Compose SDK (no shelling out to `docker compose`).
4. Watches the result for a configurable window (default 5 minutes).
5. If the stack does not stay healthy for the full window, it reverts to the last commit that was known to be healthy and watches that too.

Secrets are out of scope. ComposeLock will check that `env_file:` paths referenced in the Compose file exist, but never opens, reads, or logs them.

## Contents

- [Quick start](#quick-start) walks through the first deploy.
- [Commands](#commands) is the CLI reference.
- [How it works](#how-it-works) explains the gate, the watch, and the revert.
- [Configuration](#configuration) is the full config reference, including `compose_dir` mode.
- [Notifications](#notifications) covers the Discord embeds.
- [Keeping it running](#keeping-it-running) covers cron, `poll`, `webhook`, and the container image.
- [Development](#development) covers building and the smoke tests.

## Quick start

You need Go 1.27 or newer to build, `git` on `PATH`, and a reachable Docker daemon at runtime. The Compose SDK talks to the daemon directly, so the `docker compose` CLI is not required. To skip the build entirely, see [Run as a container](#run-as-a-container).

```sh
make build && sudo make install    # builds bin/composelock, installs to /usr/local/bin
```

Clone the repository holding your compose file, if you have not already. ComposeLock never clones on its own:

```sh
git clone <repo-url> /srv/my-stack
```

Scaffold a config and an empty state file. Both are written into the current directory, and `composelock` picks up `./composelock.json` by default, so run everything below from the same place:

```sh
cd /srv
composelock --init --with-state
```

Edit the three fields in `composelock.json` that have no useful default:

| Field          | Meaning                                                                             |
|----------------|-------------------------------------------------------------------------------------|
| `repo_path`    | Path to the local Git checkout, `/srv/my-stack` above                                |
| `compose_file` | Path to the `docker-compose.yml` to apply, relative to `repo_path`, or use `compose_dir` for a whole directory |
| `project_name` | Compose project name, lowercase letters, digits, `-` and `_` only                     |

Preview what a run would do, without touching Compose:

```sh
composelock check
```

Then deploy for real, and inspect the result:

```sh
composelock sync
composelock status
```

`sync` is one complete reconcile and exits. To keep the stack in sync continuously, point cron at it or run `poll`, see [Keeping it running](#keeping-it-running). From cron or a systemd unit, pass absolute paths: `composelock --config /srv/composelock.json sync`.

## Commands

| Command    | Description                                                            |
|------------|--------------------------------------------------------------------------|
| `sync`     | One reconcile: fetch, gate, apply, watch, revert if needed               |
| `check`    | Same as `sync` but dry run: reports what would change, applies nothing   |
| `status`   | Prints config path, state file contents, and live container health       |
| `init`     | Scaffolds a default config (and a state file with `--with-state`)        |
| `poll`     | Runs `sync` in a loop every `poll_interval_seconds`                      |
| `webhook`  | Starts an HTTP server that triggers `sync` on a Git push to `branch`     |
| `version`  | Prints version, commit, and build date                                   |

Bare `composelock` with no command is equivalent to `sync`. `--version` is also accepted as a flag and does the same as the `version` command. Flags may be written before or after the command, so `composelock init --force` and `composelock --force init` are equivalent. `poll` and `webhook` reject `--dry-run` and `--force`: they always reconcile for real.

`check` writes nothing to the state file and never touches Compose, but it does run `git fetch`, which updates `refs/remotes/<remote>/<branch>` in the repository. It takes the state lock in shared mode so it cannot read a half-moved `HEAD` while a `sync` is checking out.

### Exit codes

| Code | Meaning                                                         |
|------|-------------------------------------------------------------------|
| 0    | Success, nothing to do, or a known-bad commit was skipped       |
| 1    | Reconcile failed, but a revert brought the stack back healthy   |
| 2    | Invalid usage or configuration                                  |
| 3    | DEGRADED: revert also failed, manual intervention required      |

Once a run exits with code 3, every following run refuses to touch the stack until it is retried with `--force`.

## How it works

### The pre-flight gate

Before anything is applied, ComposeLock snapshots the currently deployed stack. If it is not healthy, the new commit is not applied at all, because deploying on top of a broken stack makes the failure impossible to attribute. A commit that already failed its health watch once is skipped the same way, so a bad commit fails once rather than on every poll tick, until a new commit lands or you pass `--force`.

The Git checkout only moves once every pre-flight check has passed, so a failed check (Docker unreachable, a missing `env_file`, an invalid Compose file) leaves the worktree untouched and is retried on the next run.

### The health watch

After `compose up`, the stack is watched for `health_watch_seconds` (default 300), polling every `health_poll_interval_seconds` (default 5). A deploy fails if a container exits non-zero, restarts more than `health_restart_tolerance` times, reports an unhealthy Docker healthcheck for `health_unhealthy_streak` consecutive polls, or is still `starting` when the window closes.

Containers that exit with code 0, such as one-shot migration or init jobs, count as completed rather than failed. Services removed from the Compose file are removed from the stack.

### Revert and DEGRADED

If the watch fails, ComposeLock checks out the last commit that was known to be healthy, re-applies it, and watches that too. A successful revert exits 1: the deploy failed, but the stack is serving again. If the revert itself fails its health watch, or there is no known-healthy commit to fall back to, the run ends DEGRADED and exits 3, and every later run refuses to act until you pass `--force`.

### Crash recovery

State (last healthy commit, last failed commit, pending commit, last result) is kept in a JSON file so a crash mid-deploy is recoverable on the next run: an interrupted deploy is re-applied and watched again, and an interrupted revert is finished. Recovery gives up after three attempts and goes DEGRADED rather than looping.

Only one reconcile touches a deployment at a time. A second process finds the state file lock held and exits 0 without doing anything, which makes it safe to run cron and a webhook server against the same config.

## Configuration

```sh
composelock --init --config composelock.json --with-state
```

This writes a default `composelock.json` and an empty `state.json`. Config path resolution order: the `--config` flag, then `$COMPOSELOCK_CONFIG`, then `./composelock.json`.

`repo_path`, `project_name`, and one of `compose_file`/`compose_dir` are required. Everything else has a sane default:

| Field                          | Default              | Meaning                                        |
|---------------------------------|-----------------------|-------------------------------------------------|
| `remote`                        | `origin`              | Git remote to fetch                             |
| `branch`                        | `main`                | Branch to track                                 |
| `compose_dir`                   | (disabled)            | Directory of compose files; overrides `compose_file` when set, see below |
| `ssh_key`                       | (system agent)        | SSH key for Git, if not using the agent         |
| `state_file`                    | `./state.json`        | Where reconcile state is stored                 |
| `retry_attempts`                | `3`                   | Git fetch retry attempts                        |
| `retry_delay_seconds`           | `20`                  | Delay between Git retries                       |
| `docker_timeout_seconds`        | `60`                  | Timeout for Docker calls other than `up`; `0` disables it |
| `docker_up_timeout_seconds`     | `1800`                | Timeout for `compose up`, which may pull or build; `0` disables it |
| `poll_interval_seconds`         | `0`                   | Used only by `composelock poll`; `0` disables it |
| `health_watch_seconds`          | `300`                 | How long to watch after apply/revert            |
| `health_poll_interval_seconds`  | `5`                   | Poll interval during the health watch           |
| `health_unhealthy_streak`       | `3`                   | Consecutive unhealthy polls before failing      |
| `health_restart_tolerance`      | `1`                   | Restarts allowed before treating it as a failure |
| `discord_webhook`               | (disabled)            | Discord webhook URL for notifications           |
| `log_format`                    | `json`                | `json` or `text`, logged to stdout              |
| `pprof_listen`                  | (disabled)            | Loopback address for a pprof server in `poll`/`webhook` mode, e.g. `127.0.0.1:6060` |
| `webhook.listen`                | `127.0.0.1:8080`      | Address for `composelock webhook`               |
| `webhook.path`                  | `/webhook`            | Path for `composelock webhook`                  |
| `webhook.secret`                | (disabled)            | HMAC secret for the webhook; empty disables validation |

Every field except `webhook.secret` can be overridden on the command line, run `composelock --help` for the full flag list. Flags win over the config file and are validated the same way. Duration flags must be whole seconds (`30s`, `5m`), a sub-second value is rejected rather than truncated.

The config file can hold `webhook.secret` and `discord_webhook`, so `--init` creates it with mode `0600`. There is deliberately no `--webhook-secret` flag, because a command line is readable by every other process on the host.

### Multiple compose files (`compose_dir`)

Instead of one `compose_file`, point `compose_dir` at a directory of compose files. When set, `compose_dir` takes precedence and `compose_file` is ignored (a warning is logged if both are set). A relative `compose_dir` is resolved against `repo_path`, so `"compose_dir": "deployment"` means `<repo_path>/deployment` regardless of which directory ComposeLock is started from.

Discovery rule: files directly inside `compose_dir` are merged into one stack named `project_name`, the same way `docker compose -f a.yaml -f b.yaml` merges multiple files into one project. Each immediate subdirectory's files are merged into their own stack, named `project_name-<subdirectory>`. Nesting deeper than one level is never scanned. For example:

```
deployment/
├── service-a.yaml
├── service-b.yaml
├── service-c.yml
└── db/
    ├── docker-compose.yaml
    └── frontend-proxy.yaml
```

produces two stacks: `project_name` (service-a/b/c merged as one project) and `project_name-db`.

Merge order within a stack is the canonical base file (`compose.yaml`, `compose.yml`, `docker-compose.yaml` or `docker-compose.yml`) first, then the rest alphabetically, then any `*.override.*` file last, so an override wins the way its name promises.

Which files are picked up:

- Only `*.yaml` and `*.yml`, and only at depth 0 or 1. Dot entries are skipped, so pointing `compose_dir` at a repository root does not turn `.github/dependabot.yml` into a stack.
- A YAML file whose top level is not Compose Spec shaped (it has no `services:` or `include:`, and holds keys the spec does not define) is some other tool's config file sitting next to a compose file, such as a `prometheus.yml`. It is skipped, not merged, and does not fail the sync. Empty files, files whose top level is a list or a scalar, and dangling symlinks are skipped the same way.
- Every remaining file is schema-validated on its own, before merging, so a file that is meant to be a compose file but is broken fails loudly with its own file name in the error instead of a generic merged-load failure. Unparsable YAML fails the same way, as does a multi-document file, since Compose would silently apply only its first document.
- Two subdirectory names that normalize to the same project name (`my db` and `my-db`) are rejected with an error, because Compose would otherwise treat both as one project and remove the other's containers.

Before anything is touched, the pre-flight gate snapshots every stack that is currently deployed, not just `project_name`, so a broken sub-stack stops a new commit from being applied on top of it.

Only the stack(s) whose files actually changed in a commit are applied and health-watched, the same skip logic described above for a single `compose_file`, generalized per stack. A change to any `*.yaml` or `*.yml` at depth 0 or 1 counts as a change to that directory's stack, including a file the commit deleted. A commit that changes no compose file at all still moves the checkout forward and is recorded as the new rollback target, so `HEAD` never falls permanently behind the branch. The changed stacks are health-watched concurrently, so a cycle costs about one watch window no matter how many stacks changed, and the first stack to fail stops the others rather than delaying the rollback until their windows expire.

If a stack's health watch fails, only the stack(s) touched in that cycle revert; a stack untouched this cycle is never affected by another stack's failure. The revert re-applies each stack from the rollback target's own file list, and a stack that only exists in the failing commit is torn down, so the deployment ends up as the rollback target describes it.

A subdirectory removed entirely has its stack torn down, but only once the cycle it was removed in is known to be healthy: if that cycle reverts, the rollback target still describes the stack and its containers stay up.

## Notifications

Set `discord_webhook` to post a colour-coded embed for every outcome that matters: a successful deploy (green), a revert that recovered the stack (orange), and a failure or DEGRADED state (red). Repeated identical notifications are suppressed for an hour per notification key, so a stack that fails every poll tick does not flood the channel, and a notification that failed to send never consumes that hour.

A successful deploy looks like this:

| Field        | Example                                                   | Meaning                                                                 |
|--------------|-----------------------------------------------------------|-------------------------------------------------------------------------|
| Commit       | `8e76297`                                                 | The commit that is now deployed                                          |
| Branch       | `main`                                                    | The tracked branch                                                       |
| Services     | `pihole, traefik, vaultwarden, whoami`                    | Every service in the stacks this cycle applied                           |
| Updated      | `vaultwarden`                                             | Only the services whose containers Compose actually replaced             |
| Stacks       | `my-stack, my-stack-db`                                   | Which stacks were applied (`compose_dir` mode only)                      |
| Health Watch | `5m0s (success)`                                          | How long the result was watched, and the verdict                         |
| Duration     | `5m36s`                                                   | Wall clock for the whole reconcile                                       |

`Services` is the full list of services in the applied stacks, while `Updated` narrows that to the ones that actually changed: Compose leaves a service alone when its configuration did not change, so a one-service bump shows seven untouched services under `Services` and a single name under `Updated`. `Updated` reads `none` when the commit touched a compose file without changing any container, and the field is left out entirely when `health_watch_seconds` is `0`, since nothing is snapshotted to compare against. The same list is logged as `updated_services`.

## Keeping it running

`composelock sync` is a single, self-contained reconcile and is meant to be invoked repeatedly: by cron, by `composelock poll` as a long-running process, or by `composelock webhook` on push. Use cron if you do not want a supervised background process; use `poll` or `webhook` if you are already running this under systemd, Docker, or a similar supervisor.

### Cron

A typical crontab entry, running every 5 minutes:

```
*/5 * * * * /usr/local/bin/composelock --config /etc/composelock/composelock.json sync >> /var/log/composelock.log 2>&1
```

Notes for cron specifically:

- Use absolute paths for the binary, `--config`, and every path inside the config file. Cron runs with a minimal environment and an unpredictable working directory.
- The user cron runs as needs permission to reach the Docker socket (usually membership in the `docker` group).
- `composelock` writes structured logs to stdout; redirecting them to a file, as above, is the simplest way to keep a record. `log_format` can be set to `text` if you prefer to read the log file directly instead of through a JSON log processor.
- Exit codes are meaningful for alerting. A wrapper script that checks `$?` after cron runs it, or cron's own `MAILTO`, is enough to get notified on exit codes 1 and 3. Discord notifications cover the same cases without needing to parse cron output at all.
- Do not run `composelock poll` from cron. `poll` is a long-running loop and will simply pile up one process per cron tick.

### Webhook

`composelock webhook` serves `webhook.path` on `webhook.listen` and reconciles on push. Set `webhook.secret` to the same value as the GitHub webhook secret so requests are authenticated with `X-Hub-Signature-256`; leaving it empty means anything that can reach the port can trigger a deploy, which is why the default listen address is loopback only. A `ping` is answered without deploying, non-push events are ignored, and a push to a branch other than `branch` is ignored. Pushes that arrive while a reconcile is running coalesce into a single follow-up run.

### Run as a container

The published image bundles the binary and `git`; it talks to the host's Docker daemon over the mounted socket, so no `docker`/`docker compose` CLI, and no `git` on the host, is needed. `repo_path` must already be a cloned checkout. ComposeLock never clones on its own, so clone once using the image's own `git` before the first run:

```sh
docker run --rm \
  -v /share/CACHEDEV1_DATA/composelock:/share/CACHEDEV1_DATA/composelock \
  --entrypoint git ghcr.io/teyhouse/composelock:latest \
  clone <repo-url> /share/CACHEDEV1_DATA/composelock/repo
```

Then run ComposeLock itself:

```sh
docker run -d --name composelock \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /share/CACHEDEV1_DATA/composelock:/share/CACHEDEV1_DATA/composelock \
  ghcr.io/teyhouse/composelock:latest \
  --config /share/CACHEDEV1_DATA/composelock/composelock.json poll
```

That one bind mount holds the repo checkout, `composelock.json`, and `state.json`, at the same path inside the container as on the host, required so bind-mount paths inside the target `docker-compose.yml` still resolve correctly, since the host daemon (not this container) creates those containers. Every tagged release publishes this image; build it locally instead with `make image`.

#### `env_file:` paths outside the mounted directory

If a service in the target compose file has an `env_file:` pointing somewhere other than inside the mounted data directory, mount that path too, at the identical location. ComposeLock and the Compose SDK read `env_file:` contents from inside this container, before anything talks to the Docker daemon. A path that only exists on the host, unmounted, fails with `env file ... not found` even though `ls` on the host finds it fine. For example, if `docker-compose.yml` has:

```yaml
services:
  traefik:
    env_file:
      - /share/secrets/traefik.env
```

add a matching mount:

```sh
docker run -d --name composelock \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /share/CACHEDEV1_DATA/composelock:/share/CACHEDEV1_DATA/composelock \
  -v /share/secrets/traefik.env:/share/secrets/traefik.env:ro \
  ghcr.io/teyhouse/composelock:latest \
  --config /share/CACHEDEV1_DATA/composelock/composelock.json poll
```

## Development

```sh
make build    # produces bin/composelock
make test     # go test -race -shuffle=on ./...
make lint     # go vet + staticcheck
make vuln     # govulncheck
make fmt      # gofmt
make smoke    # end-to-end run against the local Docker daemon
make image    # builds the container image
make install  # installs to /usr/local/bin
```

Cross-compile with `GOOS`/`GOARCH`, for example:

```sh
make build GOOS=linux GOARCH=amd64
```

### Smoke tests

`make smoke` runs the real binary end to end against the local Docker daemon. It creates a throwaway Git origin and a two-service stack (project `composelock-smoke`, image `alpine:3.22`), then deploys, crashes and reverts, skips the known-bad commit, trips the `env_file` check, fails a Git fetch, ends DEGRADED, recovers from a simulated crash, and confirms a second process is locked out. The containers and the work directory are removed afterwards.

To check Discord notifications as well, pass a webhook through the environment. It is only written into the generated config file, never into the repository:

```sh
COMPOSELOCK_SMOKE_DISCORD_WEBHOOK='https://discord.com/api/webhooks/...' make smoke
```

Set `SMOKE_KEEP=1` to keep the work directory and its logs, or `SMOKE_WORKDIR` to choose it.

`make smoke-compose-dir` covers `compose_dir` mode the same way: three independent stacks (root, `db/`, `c/`), a change touching only one stack (confirms the others are never recreated), a stack that crashes and reverts (confirms unrelated stacks stay untouched), an invalid file added anywhere in `compose_dir` (confirms the whole cycle fails loudly before touching Docker), a subdirectory removed entirely (confirms its stack is torn down via `Down` and that the commit is still recorded as the new rollback target), a non-compose YAML file next to a compose file (confirms it is skipped rather than failing the sync), and a compose file added to an existing stack that then crashes (confirms the revert re-applies that stack from the rollback target's file list instead of going DEGRADED). The same `SMOKE_KEEP`/`SMOKE_WORKDIR` knobs apply.

## Versioning

ComposeLock follows [Semantic Versioning](https://semver.org). It is currently `0.y.z`: the config format, CLI flags, and state file layout may still change without a major bump.

The `VERSION` file at the repository root always holds the version that will be used for the *next* release, for example `0.1.1`. Nobody needs to touch it for routine changes: every release workflow run tags and publishes exactly what is in `VERSION`, then increments the patch number and commits `VERSION` back to `main` for next time. Patch releases are therefore fully automatic.

Bump `VERSION` by hand only when a change is significant enough to deserve a new major or minor line, a breaking config or CLI change, for instance. Set it to whatever should be released next (`0.2.0`, `1.0.0`, ...) and commit it; the next relevant push releases exactly that version and resumes auto-incrementing the patch from there.

Once the config format, CLI surface, and state file layout are considered stable, bump `VERSION` to `1.0.0`.

## Continuous integration and releases

Every push to `main` that touches Go code (or `go.mod`/`go.sum`/the `Makefile`) runs `go vet`, `staticcheck`, the full test suite under the race detector, and `govulncheck`. Pull requests against `main` run the same checks.

Releases are manual: trigger them via the **Run workflow** button on the Release workflow, or push a version tag (e.g. `git tag v0.1.2 && git push origin v0.1.2`). The Release workflow reads `VERSION`, builds a `linux/amd64` binary, publishes a GitHub Release, builds and pushes the container image to `ghcr.io/teyhouse/composelock` (tagged with the version and `latest`), then bumps `VERSION` for the next patch.

Release notes cover every commit since the previous tag, so work can be split across as many commits as it deserves. `scripts/changelog.sh <previous-tag> <ref>` builds the list, grouping [Conventional Commit](https://www.conventionalcommits.org) subjects into breaking changes, features, fixes, documentation and maintenance. Merge commits and the workflow's own `chore: bump version` commits are left out. GitHub's own pull-request-based notes are appended underneath, so both commits and merged PRs are credited. Run the script by hand to preview what the next release will say:

```sh
scripts/changelog.sh "$(git describe --tags --abbrev=0)" HEAD
```

See `.github/workflows/ci.yml` and `.github/workflows/release.yml`.

## AI Development Disclosure

This project was developed with the assistance of AI/LLM tools as part of the development workflow.

**Human Oversight:** All AI-generated code was manually reviewed, tested, modified, and optimized by the project maintainers. The final implementation reflects human judgment and understanding of the project requirements.
