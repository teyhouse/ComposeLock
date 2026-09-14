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
2. Refuses to apply anything if the running stack is not healthy, or if the new commit already failed its health check once before.
3. Applies the new commit through the official Docker Compose SDK (no shelling out to `docker compose`).
4. Watches the result for a configurable window (default 5 minutes).
5. Reverts to the last commit known to be healthy, and watches that too, if the stack does not stay healthy for the full window.

Secrets are out of scope. ComposeLock checks that `env_file:` paths exist, but never opens, reads, or logs them.

## Contents

- [Quick start](#quick-start): run it as a container, or install the binary.
- [Commands](#commands): CLI reference and exit codes.
- [How it works](#how-it-works): the gate, the watch, the revert, crash recovery.
- [Configuration](#configuration): full reference, including `compose_dir` mode.
- [Notifications](#notifications): the Discord embeds.
- [Keeping it running](#keeping-it-running): cron, `poll`, `webhook`.
- [Development](#development): building and the smoke tests.

## Quick start

ComposeLock never clones for you: `repo_path` must already be a checkout of the repository holding your compose file.

### Run as a container

The image bundles the binary and `git` and reaches the host's Docker daemon over the mounted socket, so the host needs neither `git` nor the `docker`/`docker compose` CLI. Clone once with the image's own `git`:

```sh
docker run --rm \
  -v /srv/composelock:/srv/composelock \
  --entrypoint git ghcr.io/teyhouse/composelock:latest \
  clone <repo-url> /srv/composelock/repo
```

Put a `composelock.json` beside that checkout (see [Configuration](#configuration)), then run ComposeLock:

```sh
docker run -d --name composelock \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /srv/composelock:/srv/composelock \
  ghcr.io/teyhouse/composelock:latest \
  --config /srv/composelock/composelock.json poll
```

That one bind mount holds the repo checkout, `composelock.json` and `state.json`, and must sit at the same path inside the container as on the host: the host daemon, not this container, creates the target containers, so bind-mount paths in your `docker-compose.yml` have to resolve identically on both sides. Every tagged release publishes the image; `make image` builds it locally.

An `env_file:` outside that mounted directory has to be mounted too, at the identical path. ComposeLock reads `env_file:` contents from inside this container, before anything reaches the daemon, so an unmounted host path fails with `env file ... not found` even though `ls` on the host finds it:

```sh
  -v /share/secrets/traefik.env:/share/secrets/traefik.env:ro
```

### Install the binary

Needs Go 1.27 or newer to build, `git` on `PATH`, and a reachable Docker daemon at runtime. The Compose SDK talks to the daemon directly, so the `docker compose` CLI is not required.

```sh
make build && sudo make install    # builds bin/composelock, installs to /usr/local/bin
git clone <repo-url> /srv/my-stack # unless it is already cloned
```

### First deploy

Scaffold a config and an empty state file. Both are written into the current directory, and `composelock` picks up `./composelock.json` by default, so run everything from the same place:

```sh
cd /srv
composelock --init --with-state
```

Edit the three fields with no useful default:

| Field          | Meaning                                                                             |
|----------------|-------------------------------------------------------------------------------------|
| `repo_path`    | Path to the local Git checkout, `/srv/my-stack` above                                |
| `compose_file` | Path to the `docker-compose.yml` to apply, relative to `repo_path`, or use `compose_dir` for a whole directory |
| `project_name` | Compose project name, lowercase letters, digits, `-` and `_` only                     |

```sh
composelock check     # dry run, touches nothing
composelock sync
composelock status
```

The first `sync` deploys the current checkout even though `HEAD` already matches the branch, so a fresh install needs no throwaway commit. `sync` is one complete reconcile and exits; to stay in sync, use cron, `poll` or `webhook`, see [Keeping it running](#keeping-it-running). From cron or systemd, pass absolute paths: `composelock --config /srv/composelock.json sync`.

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

Bare `composelock` means `sync`, and `--version` matches the `version` command. Flags may come before or after the command. `poll` and `webhook` reject `--dry-run` and `--force`: they always reconcile for real.

`check` writes no state and never touches Compose, but it does run `git fetch`, which updates `refs/remotes/<remote>/<branch>`. It exits 1 when the gate would block the commit, so a green `check` in CI means "would deploy", not merely "parsed". `check` and `status` take the state lock in shared mode, so neither reads a half-moved `HEAD` mid-checkout; `status` prints a note and reports anyway if a reconcile holds the lock.

### Exit codes

| Code | Meaning                                                         |
|------|-------------------------------------------------------------------|
| 0    | Success, nothing to do, or a known-bad commit was skipped       |
| 1    | Reconcile failed, but a revert brought the stack back healthy   |
| 2    | Invalid usage or configuration                                  |
| 3    | DEGRADED: revert also failed, manual intervention required      |

After an exit 3, every following run refuses to touch the stack until it is retried with `--force`.

## How it works

### What counts as a change

A relative `compose_file` resolves against `repo_path`, like `compose_dir` does, so it means the same file wherever ComposeLock is started from.

A commit only deploys when it touches something the deployment depends on. In `compose_file` mode that is the compose file plus every input reachable from it: `include:` fragments and their nested includes, `extends.file` targets, each `env_file`, the implicit `.env` beside the compose file and beside each included project, every service build context and Dockerfile, and any `config`/`secret` sourced from a file. A tracked `.env`, Dockerfile or fragment therefore redeploys; a docs-only commit does not. Services declaring `build:` are rebuilt on every apply rather than only when their image is missing, so a Dockerfile-only commit reaches the running container instead of passing its watch against the previous image.

Two cases apply rather than skip, since a wrong skip is silent and a wrong apply is not: a project that fails to load at all, and an input set that cannot be enumerated statically, such as an `include:` path built from a variable (`${STACK_DIR}/compose.yaml`). Remote `include:` references (`https://`, `git@`, `oci://`) are ignored, since they can never be a path in this repository. `compose_dir` mode uses a broader and cheaper rule, see [Multiple compose files](#multiple-compose-files-compose_dir).

An irrelevant commit still moves the checkout forward, so `HEAD` never falls behind the branch, but it is recorded in `last_checkout_commit`, not `last_healthy_commit`: the rollback target is only ever a commit that was applied and passed a watch.

### The pre-flight gate

Before applying anything, ComposeLock snapshots the deployed stack. If it is unhealthy the new commit is not applied at all, because a failure on top of a broken stack cannot be attributed. The last healthy commit is re-applied and watched instead, and three blocked runs in a row end DEGRADED rather than repeating a full watch window forever. A commit that already failed is skipped the same way, so a bad commit fails once rather than every poll tick, until a new commit lands or you pass `--force`. The last several failed commits are remembered, so two bad commits do not take turns being retried.

The checkout only moves once the gate has passed, so a gate failure (Docker unreachable) leaves the worktree untouched for the next run. Discovery failing at the *current* checkout is deliberately not a gate failure: a broken compose file already checked out would otherwise wedge the deployment there forever, since the commit repairing it could never be checked out. Such a run warns, falls back to `last_healthy_stacks` for the gate, and carries on, so `git checkout --detach --force` repairs the worktree.

Checks that need the new tree (discovering its stacks, loading each project, verifying every `env_file` exists) run right after the checkout and before Compose is touched. If one fails, the worktree is restored and the commit recorded as known-bad. Push a new commit or run `composelock sync --force` to retry, which is what you want after fixing an `env_file` that lives on the host rather than in the repository.

`git checkout` runs `--detach --force`: the worktree is a deployment artifact and the branch is the only source of truth, so local edits under `repo_path` are discarded rather than carried into the next deploy.

### The health watch

After `compose up` the stack is watched for `health_watch_seconds` (default 300), polling every `health_poll_interval_seconds` (default 5, never larger than the window). At least one poll always follows the baseline, so the verdict is never the snapshot taken the instant `Up` returned, where a container legitimately still reads as `starting`.

A deploy fails when a container:

- exits non-zero, or lands in `dead` or `removing`, on the poll that sees it.
- restarts more than `health_restart_tolerance` times. Counted per container against its own restart count at the start of the watch, so a crash-looping replica cannot hide behind a sibling that restarted more often in the past.
- reports an unhealthy healthcheck for `health_unhealthy_streak` consecutive polls. Streaks are per container, so two services unhealthy on alternate polls do not add up to a streak neither of them had.
- is not running at all, whether `created`, `paused`, `restarting`, or an unrecognised state, for `health_unhealthy_streak` consecutive polls, instead of burning the whole window first.
- reports a healthcheck value other than `healthy`, `none` or `starting`, which counts as unhealthy so an unrecognised one fails closed.
- disappears. Every baseline container must still be present at every poll, so losing one of two replicas fails even though the service name remains, as does a container swapped for a fresh one with a new id, which would otherwise read as zero restarts and pass.
- is still `starting` when the window closes.

Scaling up mid-window is not a failure. Containers that exit 0, such as one-shot migration or init jobs, count as completed, including the healthcheck they last reported, so a finished job whose final probe was unhealthy still passes. Services removed from the Compose file are removed from the stack.

A `restarting` container is judged by its restart count, not by the probe it reported before restarting, which is stale: within tolerance it passes the final verdict and the gate, so ordinary `restart: always` churn neither rolls back a healthy deploy nor blocks the next one. It does not pass indefinitely, though. Still `restarting` on `health_unhealthy_streak` consecutive polls counts as wedged, because a slow crash loop can stay inside the restart tolerance for a whole window while never serving anything.

### Revert and DEGRADED

If the watch fails, ComposeLock checks out the last healthy commit, re-applies it, and watches that too. The revert loads every stack and verifies its `env_file` paths before the first `compose up`, so a rollback that cannot succeed fails cleanly instead of leaving half the stacks at the target and half at the failed commit. A successful revert exits 1: the deploy failed, but the stack is serving again. If the revert fails its own watch, or there is no healthy commit to fall back to, the run ends DEGRADED and exits 3.

Infrastructure failures are treated differently from bad commits. A daemon restart, a timeout or a connection refused during `compose up` neither marks the commit bad nor reverts into the same broken daemon: the apply is left pending for the next run, which is what would have recovered it anyway. The same holds during a revert, and a revert interrupted before its watch finishes, by a shutdown or a blip, is recorded as pending and resumed rather than forgotten.

### Crash recovery

State (last healthy commit and the stacks it deployed, last failed commit, last checkout, pending commit and the stacks it was applying, last result) lives in a JSON file, so a crash mid-deploy is recoverable: an interrupted deploy is re-applied and watched again, an interrupted revert is finished, both limited to the stacks that run was touching. Every recovery run fetches the branch first, so a commit pushed to repair a failing deploy is applied instead of the pending one rather than staying invisible.

Recovery gives up and goes DEGRADED rather than looping, after three attempts or once the pending commit has been unresolved for longer than the recovery budget, whichever comes first. The budget is three times the watch window plus 15 minutes of grace per attempt, so an hour at the defaults. Only a real apply or revert spends an attempt: a Docker blip or an unreadable `compose_dir` returns without consuming one. Going DEGRADED clears the pending block and records the in-flight commit in `last_attempt_commit` and `failed_commits` rather than replaying it, so `composelock sync --force` resets the counter and takes the branch as it stands now.

Teardown and checkout restore run to completion through SIGINT or SIGTERM, because a half-removed stack is worse than a slow exit. Stacks are torn down concurrently, up to eight at a time, so shutdown during a teardown is bounded by `docker_timeout_seconds` per stack (capped at two minutes each) plus 30 seconds for the checkout restore, rather than by the sum over every stack. `sync` and `poll` run the reconcile to the end; `webhook` stops accepting triggers, then waits for the in-flight run.

Only one reconcile touches a deployment at a time. A second process finds the state file lock held and exits 0, which makes it safe to run cron and a webhook server against the same config.

## Configuration

```sh
composelock --init --config composelock.json --with-state
```

This writes a default `composelock.json` and an empty `state.json`. Config path resolution order: the `--config` flag, then `$COMPOSELOCK_CONFIG`, then `./composelock.json`.

`repo_path`, `project_name`, and one of `compose_file`/`compose_dir` are required. `remote` and `branch` allow only letters, digits, `.`, `_`, `/` and `-`, and may not start with `-` or contain `..`, since both are passed straight to `git`. A config whose `schema_version` is newer than the binary understands is refused rather than loaded with its new fields ignored, and unknown fields are warned about by full dotted path, so `webhook.secrt` is reported instead of quietly leaving the webhook unauthenticated. Everything else has a sane default:

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

Every field except `webhook.secret` can be overridden on the command line (`composelock --help` lists the flags). Flags win over the config file and are validated the same way; duration flags must be whole seconds (`30s`, `5m`), a sub-second value being rejected rather than truncated. Since the file can hold `webhook.secret` and `discord_webhook`, `--init` creates it with mode `0600`. There is deliberately no `--webhook-secret` flag, because a command line is readable by every other process on the host.

### Multiple compose files (`compose_dir`)

Point `compose_dir` at a directory of compose files instead of naming one `compose_file`. It takes precedence when both are set (with a warning), and a relative value resolves against `repo_path`, so `"compose_dir": "deployment"` means `<repo_path>/deployment`.

Files directly inside `compose_dir` merge into one stack named `project_name`, the way `docker compose -f a.yaml -f b.yaml` merges files into one project. Each immediate subdirectory merges into its own stack, named `project_name-<subdirectory>`. Deeper nesting is never scanned:

```
deployment/
├── service-a.yaml
├── service-b.yaml
├── service-c.yml
└── db/
    ├── docker-compose.yaml
    └── frontend-proxy.yaml
```

That produces two stacks: `project_name` (service-a/b/c as one project) and `project_name-db`. Within a stack, the canonical base file (`compose.yaml`, `compose.yml`, `docker-compose.yaml` or `docker-compose.yml`) merges first, then the rest alphabetically, then any `*.override.*` file last, so an override wins the way its name promises.

Which files are picked up:

- Only `*.yaml` and `*.yml`, only at depth 0 or 1, skipping dot entries, so pointing `compose_dir` at a repository root does not turn `.github/dependabot.yml` into a stack.
- A YAML file that is not Compose Spec shaped (no `services:` or `include:`, plus keys the spec does not define) is another tool's config sitting next to a compose file, such as a `prometheus.yml`. It is skipped rather than merged and does not fail the sync, with a warning naming the unrecognized keys so a typo in a real top-level key stays visible. Empty files, top-level lists or scalars, and dangling symlinks are skipped the same way. A file that starts as a mapping and then holds a second, non-mapping document is an error rather than a skip, because dropping it would take the whole stack out of discovery.
- A directory whose files define no `services:` and no `include:` between them is skipped rather than becoming a stack that can only report `NO CONTAINERS`. Fragments contributing only `networks:` or `volumes:` still merge alongside a sibling that does define services.
- Every remaining file is schema-validated on its own before merging, so a broken compose file fails with its own name in the error instead of a generic merged-load failure. Unparsable YAML fails the same way, as does a multi-document file, since Compose would silently apply only its first document.
- Two subdirectories that normalize to the same project name (`my db` and `my-db`) are rejected, because Compose would treat both as one project and remove the other's containers.
- A missing `compose_dir` is an error, not an empty deployment. An existing but empty one is legal, since a commit may remove the last stack, but a typo, or a bind mount that is not mounted in container mode, must not be mistaken for one and recorded as a healthy deploy of nothing.

The gate snapshots every currently deployed stack, not just `project_name`, so a broken sub-stack stops a new commit landing on top of it. Only the stacks whose files changed are then applied and watched:

- Any changed file under a stack's directory counts, at any depth and including dot files, because `env_file` targets, Dockerfiles, build contexts and `include:` fragments all change what the stack deploys.
- A file belongs to the most deeply nested stack directory containing it, so a change under `db/` never re-ups the root stack, and files under a deleted subdirectory belong to that removed stack rather than its parent.
- A file outside `compose_dir` counts when it is a real input of a stack, such as an `include:` fragment or an `env_file` reached through `../shared/`. That check loads the project, so it only decides stacks the directory rule did not already select.
- A stack whose inputs cannot be enumerated, such as an `include:` path built from a variable, counts as changed, the same fallback single-file mode makes.
- A stack whose project cannot be loaded at all keeps the directory rule alone rather than joining an unrelated stack's cycle. A cycle left undecided that way neither becomes the rollback target nor records its checkout: leaving `last_checkout_commit` behind is what makes the next run see the drift and weigh the stack again, instead of treating a commit it never judged as settled.

Changed stacks are watched concurrently, so a cycle costs about one watch window however many changed, and the first failure stops the others rather than delaying the rollback until their windows expire. Only the stacks touched in that cycle revert, each re-applied from the rollback target's own file list, and a stack that exists only in the failing commit is torn down, so the deployment ends up as the rollback target describes it. A stack whose subdirectory was removed is torn down only once the cycle removing it is known to be healthy: if that cycle reverts, the target still describes the stack and its containers stay up.

## Notifications

Set `discord_webhook` to post a colour-coded embed for every outcome that matters: a successful deploy (green), a revert that recovered the stack (orange), and a failure or DEGRADED state (red). Repeated identical notifications are suppressed for an hour per key, so a stack failing every poll tick does not flood the channel, and a notification that failed to send never consumes that hour. The key names the event and its commit, and a pre-flight DEGRADED (which has no failing commit of its own) is keyed by its rollback target, so two of them do not silence each other.

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

Compose leaves a service alone when its configuration did not change, so `Updated` narrows `Services` to what actually moved: a one-service bump lists seven names under `Services` and one under `Updated`. It reads `none` when a compose file changed without changing any container, and is omitted entirely when `health_watch_seconds` is `0`, since there is nothing to compare against. The same list is logged as `updated_services`.

## Keeping it running

`composelock sync` is a single self-contained reconcile, meant to be invoked repeatedly: by cron, by `composelock poll` as a long-running process, or by `composelock webhook` on push. Use cron if you do not want a supervised background process; use `poll` or `webhook` under systemd, Docker or a similar supervisor, as the [container example](#run-as-a-container) does.

### Cron

Running every 5 minutes:

```
*/5 * * * * /usr/local/bin/composelock --config /etc/composelock/composelock.json sync >> /var/log/composelock.log 2>&1
```

- Use absolute paths for the binary, `--config`, and every path inside the config. Cron runs with a minimal environment and an unpredictable working directory.
- The cron user needs access to the Docker socket, usually via the `docker` group.
- Logs go to stdout; redirecting to a file is the simplest record. Set `log_format` to `text` to read it without a JSON processor.
- Exit codes drive alerting: a wrapper checking `$?`, or cron's `MAILTO`, covers exit 1 and 3. Discord notifications cover the same cases without parsing output.
- Never run `poll` from cron. It is a long-running loop and will pile up one process per tick.

### Webhook

`composelock webhook` serves `webhook.path` on `webhook.listen` and reconciles on push. Set `webhook.secret` to the GitHub webhook secret so requests are authenticated with `X-Hub-Signature-256`. An empty secret lets anything that reaches the port trigger a deploy, so it is only accepted on a loopback `webhook.listen`; a non-loopback address without a secret is rejected at startup. To expose it publicly, either set a secret or keep the listener on loopback behind a reverse proxy. A `ping` is answered without deploying, non-push events and pushes to other branches are ignored, and pushes arriving during a reconcile coalesce into one follow-up run.

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

Cross-compile with `GOOS`/`GOARCH`, for example `make build GOOS=linux GOARCH=amd64`.

### Smoke tests

`make smoke` runs the real binary end to end against the local Docker daemon, on a throwaway Git origin and a two-service stack (project `composelock-smoke`, image `alpine:3.22`). It deploys, crashes and reverts, skips the known-bad commit, trips the `env_file` check, fails a Git fetch, ends DEGRADED, recovers from a simulated crash, and confirms a second process is locked out. Containers and the work directory are removed afterwards.

`make smoke-compose-dir` covers `compose_dir` mode across three stacks (root, `db/`, `c/`): a change touching one stack leaves the others unrecreated, a crashing stack reverts without disturbing the rest, an invalid file anywhere fails the cycle loudly before Docker is touched, a removed subdirectory is torn down while its commit still becomes the rollback target, a non-compose YAML beside a compose file is skipped rather than failing the sync, and a compose file added to a stack that then crashes reverts from the rollback target's file list instead of going DEGRADED.

Set `SMOKE_KEEP=1` to keep the work directory and its logs, or `SMOKE_WORKDIR` to choose it. To exercise Discord too, pass a webhook through the environment; it is written only into the generated config, never into the repository:

```sh
COMPOSELOCK_SMOKE_DISCORD_WEBHOOK='https://discord.com/api/webhooks/...' make smoke
```

## Versioning

ComposeLock follows [Semantic Versioning](https://semver.org). It is currently `0.y.z`: the config format, CLI flags, and state file layout may still change without a major bump, and `VERSION` goes to `1.0.0` once they are stable.

The `VERSION` file holds the version for the *next* release. Routine changes never touch it: each release workflow run tags and publishes what is in `VERSION`, then increments the patch and commits it back to `main`, so patch releases are automatic. Bump it by hand only for a new major or minor line, such as a breaking config or CLI change: set what should be released next (`0.2.0`, `1.0.0`, ...) and commit, and the next release uses exactly that before resuming patch increments.

## Continuous integration and releases

Every push to `main` touching Go code (or `go.mod`/`go.sum`/the `Makefile`) runs `go vet`, `staticcheck`, the full suite under the race detector, and `govulncheck`. Pull requests run the same checks.

Releases are manual: use the **Run workflow** button on the Release workflow, or push a version tag (`git tag v0.1.2 && git push origin v0.1.2`). The workflow reads `VERSION`, builds a `linux/amd64` binary, publishes a GitHub Release, pushes the image to `ghcr.io/teyhouse/composelock` (tagged with the version and `latest`), then bumps `VERSION`.

Release notes cover every commit since the previous tag, so work can be split across as many commits as it deserves. `scripts/changelog.sh <previous-tag> <ref>` groups [Conventional Commit](https://www.conventionalcommits.org) subjects into breaking changes, features, fixes, documentation and maintenance, leaving out merge commits and the workflow's own version bumps. GitHub's pull-request notes are appended underneath, so both commits and merged PRs are credited. Preview the next release with:

```sh
scripts/changelog.sh "$(git describe --tags --abbrev=0)" HEAD
```

See `.github/workflows/ci.yml` and `.github/workflows/release.yml`.

## AI Development Disclosure

This project was developed with the assistance of AI/LLM tools as part of the development workflow.

**Human Oversight:** All AI-generated code was manually reviewed, tested, modified, and optimized by the project maintainers. The final implementation reflects human judgment and understanding of the project requirements.
