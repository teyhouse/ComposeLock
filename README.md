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

The first `sync` deploys the current checkout even though `HEAD` already matches the branch, so a fresh install does not need a throwaway commit to get going. `sync` is one complete reconcile and exits. To keep the stack in sync continuously, point cron at it or run `poll`, see [Keeping it running](#keeping-it-running). From cron or a systemd unit, pass absolute paths: `composelock --config /srv/composelock.json sync`.

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

`check` writes nothing to the state file and never touches Compose, but it does run `git fetch`, which updates `refs/remotes/<remote>/<branch>` in the repository. It exits 1 when the pre-flight gate would block the commit, so a green `check` in CI means "would deploy", not merely "parsed".

`check` and `status` take the state lock in shared mode so neither can read a half-moved `HEAD` while a `sync` is checking out. `status` prints a note and reports anyway if a reconcile is holding the lock, rather than failing.

### Exit codes

| Code | Meaning                                                         |
|------|-------------------------------------------------------------------|
| 0    | Success, nothing to do, or a known-bad commit was skipped       |
| 1    | Reconcile failed, but a revert brought the stack back healthy   |
| 2    | Invalid usage or configuration                                  |
| 3    | DEGRADED: revert also failed, manual intervention required      |

Once a run exits with code 3, every following run refuses to touch the stack until it is retried with `--force`.

## How it works

### What counts as a change

A relative `compose_file` is resolved against `repo_path`, the same way a relative `compose_dir` is, so it means the same file regardless of which directory ComposeLock is started from.

A commit only triggers a deploy when it touches something the deployment actually depends on. In `compose_file` mode that is the compose file itself plus every input reachable from it: `include:` fragments and their own nested includes, `extends.file` targets, each `env_file`, the implicit `.env` beside the compose file and beside each included project, every service build context and Dockerfile, and any `config`/`secret` sourced from a file. A change to a tracked `.env`, a Dockerfile or an included fragment therefore redeploys, while a docs-only commit does not.

Two cases fall back to applying rather than skipping, on the principle that a wrong skip is silent and a wrong apply is not: the project failing to load at all, and an input set that cannot be enumerated statically, such as an `include:` path built from a variable (`${STACK_DIR}/compose.yaml`) or a fragment that cannot be read. Remote `include:` references (`https://`, `git@`, `oci://`) are ignored, since they can never be a path in this repository. In `compose_dir` mode the rule is broader and cheaper, see [Multiple compose files](#multiple-compose-files-compose_dir).

A commit that changes nothing relevant still moves the checkout forward, so `HEAD` never falls permanently behind the branch, but it is recorded in `last_checkout_commit` rather than `last_healthy_commit`: the rollback target is only ever a commit that was applied and passed a health watch.

### The pre-flight gate

Before anything is applied, ComposeLock snapshots the currently deployed stack. If it is not healthy, the new commit is not applied at all, because deploying on top of a broken stack makes the failure impossible to attribute. Instead the last healthy commit is re-applied and watched, and if the gate blocks three runs in a row the deployment goes DEGRADED rather than repeating a full watch window every tick forever.

A commit that already failed is skipped the same way, so a bad commit fails once rather than on every poll tick, until a new commit lands or you pass `--force`. The last several failed commits are remembered, not just the most recent one, so two bad commits in a row do not take turns being retried.

The Git checkout only moves once the pre-flight snapshot has passed, so a gate failure (Docker unreachable) leaves the worktree untouched and is retried on the next run. Discovery failing at the *current* checkout is deliberately not a gate failure: an invalid compose file that is already checked out would otherwise wedge the deployment at the broken commit forever, since the commit that repairs it could never be checked out. Such a run warns, falls back to the stacks in `last_healthy_stacks` for the gate, and carries on, so `git checkout --detach --force` repairs the worktree. Checks that can only run against the new tree, discovering its stacks, loading each project, and verifying every `env_file` exists, run right after the checkout and before Compose is touched. If one of them fails, the worktree is restored to the previous commit and the commit is recorded as known-bad, so it fails once rather than on every poll tick. Push a new commit or run `composelock sync --force` to retry it, which is what you want after fixing an `env_file` that lives on the host rather than in the repository.

`git checkout` runs with `--detach --force`: the worktree is a deployment artifact, and the tracked branch is the only source of truth, so uncommitted local edits under `repo_path` are discarded rather than silently carried into the next deploy.

### The health watch

After `compose up`, the stack is watched for `health_watch_seconds` (default 300), polling every `health_poll_interval_seconds` (default 5). At least one poll always happens after the baseline, so the verdict is never the snapshot taken the instant `Up` returned, where a container legitimately still reads as `starting`. `health_poll_interval_seconds` may not exceed `health_watch_seconds`. A deploy fails if a container exits non-zero, restarts more than `health_restart_tolerance` times, reports an unhealthy Docker healthcheck for `health_unhealthy_streak` consecutive polls, loses a replica, or is still `starting` when the window closes. A container that lands in `dead` or `removing` fails the watch on the poll that sees it, and one wedged in `created` or `paused` fails after `health_unhealthy_streak` polls, rather than each of them burning the whole window before the rollback starts.

`health_restart_tolerance` is counted per container, against that container's own restart count when the watch started, so one crash-looping replica of a service cannot hide behind a sibling that happens to have restarted more often in the past. Every container present at the baseline must still be present at every poll: losing one of two replicas fails the watch even though the service name is still there, and so does a container being swapped for a fresh one with a new id, which would otherwise read as zero restarts and pass. Scaling up mid-window is not a failure.

Containers that exit with code 0, such as one-shot migration or init jobs, count as completed rather than failed, including the healthcheck they last reported: a finished job whose final probe was unhealthy does not fail the watch. Services removed from the Compose file are removed from the stack. A container that is `restarting` passes at every poll, at the final verdict and at the pre-flight gate, as long as its restart count is still within tolerance. The healthcheck it reported before it restarted is stale, so restart churn is judged by the restart count alone and ordinary `restart: always` churn neither rolls back a healthy deploy nor blocks the next one.

### Revert and DEGRADED

A failure that is the infrastructure's rather than the commit's, a Docker daemon restart, a timeout, or a connection refused during `compose up`, does not mark the commit bad and does not trigger a revert into the same broken daemon. The apply is left pending and the next run picks it up, which is what would have recovered it anyway.

If the watch fails, ComposeLock checks out the last commit that was known to be healthy, re-applies it, and watches that too. The revert loads every stack and verifies its `env_file` paths before the first `compose up`, the same way the apply path does, so a rollback that cannot succeed fails cleanly instead of leaving half the stacks at the target and half at the failed commit. A successful revert exits 1: the deploy failed, but the stack is serving again. If the revert itself fails its health watch, or there is no known-healthy commit to fall back to, the run ends DEGRADED and exits 3, and every later run refuses to act until you pass `--force`.

If the revert is interrupted before its watch finishes, by a shutdown or a Docker blip, the run records the revert as still pending and the next run resumes it, rather than forgetting it happened.

### Crash recovery

State (last healthy commit and the stacks it deployed, last failed commit, last checkout, pending commit and the stacks it was applying, last result) is kept in a JSON file so a crash mid-deploy is recoverable on the next run: an interrupted deploy is re-applied and watched again, and an interrupted revert is finished, in both cases limited to the stacks the interrupted run was touching. Recovery gives up and goes DEGRADED rather than looping, after three attempts or once the pending commit has been unresolved for longer than the recovery budget, whichever comes first. The budget is three times the health watch window plus 15 minutes of grace per attempt, so an hour with the default five minute watch. Only a real apply or revert attempt spends one of the three: a Docker blip or an unreadable `compose_dir` returns without consuming the budget. `composelock sync --force` resets the counter, which is how a DEGRADED deployment is retried.

Teardown and checkout restore deliberately run to completion through a SIGINT or SIGTERM, because a half-removed stack is worse than a slow exit. Stacks are torn down concurrently, up to eight at a time, so shutdown during a teardown is bounded by `docker_timeout_seconds` per stack, capped at two minutes each, plus 30 seconds for the checkout restore, rather than by the sum over every stack. All three modes share that contract: `sync` and `poll` run the reconcile to the end, and `webhook` stops accepting triggers and then waits for the in-flight reconcile before the process exits.

Only one reconcile touches a deployment at a time. A second process finds the state file lock held and exits 0 without doing anything, which makes it safe to run cron and a webhook server against the same config.

## Configuration

```sh
composelock --init --config composelock.json --with-state
```

This writes a default `composelock.json` and an empty `state.json`. Config path resolution order: the `--config` flag, then `$COMPOSELOCK_CONFIG`, then `./composelock.json`.

`repo_path`, `project_name`, and one of `compose_file`/`compose_dir` are required. `remote` and `branch` are restricted to letters, digits, `.`, `_`, `/` and `-`, and may not start with `-` or contain `..`, since both are passed straight to `git`. A config carrying a `schema_version` newer than the binary understands is refused rather than silently loaded with its new fields ignored, and unknown fields are warned about by their full dotted path, so `webhook.secrt` is reported instead of quietly leaving the webhook unauthenticated. Everything else has a sane default:

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
| `webhook.secret`                | (disabled)            | HMAC secret for the webhook; empty disables validation, and is only allowed on a loopback `webhook.listen` |

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
- A YAML file whose top level is not Compose Spec shaped (it has no `services:` or `include:`, and holds keys the spec does not define) is some other tool's config file sitting next to a compose file, such as a `prometheus.yml`. It is skipped, not merged, and does not fail the sync, with a warning naming the keys that were not recognized so a typo in a real top-level key is visible instead of silent. Empty files, files whose top level is a list or a scalar, and dangling symlinks are skipped the same way. A file that starts as a mapping and then holds a second, non-mapping document is an error rather than a skip, because dropping it would take the whole stack out of discovery.
- A directory whose compose files define no `services:` and no `include:` between them is skipped rather than turned into a stack that can only ever report `NO CONTAINERS`. Fragment files that contribute only `networks:` or `volumes:` still merge into a stack alongside a sibling that does define services.
- Every remaining file is schema-validated on its own, before merging, so a file that is meant to be a compose file but is broken fails loudly with its own file name in the error instead of a generic merged-load failure. Unparsable YAML fails the same way, as does a multi-document file, since Compose would silently apply only its first document.
- Two subdirectory names that normalize to the same project name (`my db` and `my-db`) are rejected with an error, because Compose would otherwise treat both as one project and remove the other's containers.
- A `compose_dir` that does not exist is an error, not an empty deployment. A directory that exists and holds no compose files stays legal, since a commit may remove the last stack, but a typo in `compose_dir`, or a bind mount that is not mounted in container mode, must not be mistaken for one and recorded as a healthy deploy of nothing.

Before anything is touched, the pre-flight gate snapshots every stack that is currently deployed, not just `project_name`, so a broken sub-stack stops a new commit from being applied on top of it.

In `compose_dir` mode, only the stack(s) whose files actually changed in a commit are applied and health-watched. Any changed file under a stack's directory counts, at any depth and including dot files, because `env_file` targets, Dockerfiles, build contexts and `include:` fragments all change what the stack deploys even though none of them is a compose file. A file is attributed to the most deeply nested stack directory that contains it, so a change under `db/` never re-ups the root stack as well, and files under a subdirectory that the commit deleted belong to that removed stack rather than to its parent. A commit that touches nothing under `compose_dir` still moves the checkout forward, so `HEAD` never falls permanently behind the branch, but it does not become the rollback target: `last_healthy_commit` only ever advances to a commit that was applied and passed a health watch, and `last_checkout_commit` records where `HEAD` was left. The changed stacks are health-watched concurrently, so a cycle costs about one watch window no matter how many stacks changed, and the first stack to fail stops the others rather than delaying the rollback until their windows expire.

If a stack's health watch fails, only the stack(s) touched in that cycle revert; a stack untouched this cycle is never affected by another stack's failure. The revert re-applies each stack from the rollback target's own file list, and a stack that only exists in the failing commit is torn down, so the deployment ends up as the rollback target describes it.

A subdirectory removed entirely has its stack torn down, but only once the cycle it was removed in is known to be healthy: if that cycle reverts, the rollback target still describes the stack and its containers stay up.

## Notifications

Set `discord_webhook` to post a colour-coded embed for every outcome that matters: a successful deploy (green), a revert that recovered the stack (orange), and a failure or DEGRADED state (red). Repeated identical notifications are suppressed for an hour per notification key, so a stack that fails every poll tick does not flood the channel, and a notification that failed to send never consumes that hour. The key names the event and the commit it is about, and a pre-flight DEGRADED (which has no failing commit of its own) is keyed by its rollback target, so two different ones do not silence each other.

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

`composelock webhook` serves `webhook.path` on `webhook.listen` and reconciles on push. Set `webhook.secret` to the same value as the GitHub webhook secret so requests are authenticated with `X-Hub-Signature-256`. Leaving it empty means anything that can reach the port can trigger a deploy, so it is only accepted on a loopback `webhook.listen`; a non-loopback listen address without a secret is rejected at startup. To expose the webhook publicly, either set a secret or keep the listener on loopback and front it with a reverse proxy. A `ping` is answered without deploying, non-push events are ignored, and a push to a branch other than `branch` is ignored. Pushes that arrive while a reconcile is running coalesce into a single follow-up run.

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
