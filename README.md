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

## Documentation

- [How it works](docs/how-it-works.md): what counts as a change, the pre-flight gate, the health watch, revert and DEGRADED, crash recovery.
- [Configuration](docs/configuration.md): every field with its default, and the command-line overrides.
- [Multiple compose files](docs/compose-dir.md): `compose_dir` mode, stack discovery and per-stack change detection.
- [Notifications](docs/notifications.md): the Discord embeds.
- [Keeping it running](docs/running.md): cron, `poll`, `webhook`.
- [Development](docs/development.md): building, the smoke tests, versioning and releases.

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

Put a `composelock.json` beside that checkout (see [Configuration](docs/configuration.md)), then run ComposeLock:

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
| `status`   | Prints config path, pause state, recent deployments, state file contents, and live container health |
| `init`     | Scaffolds a default config (and a state file with `--with-state`)        |
| `poll`     | Runs `sync` in a loop every `poll_interval_seconds`                      |
| `webhook`  | Starts an HTTP server that triggers `sync` on a Git push to `branch`     |
| `pause`    | Holds deployments until `resume` (or for `-for 2h`), with an optional `-reason` |
| `resume`   | Lifts a pause                                                            |
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

## Keeping it running

`composelock sync` is a single self-contained reconcile, meant to be invoked repeatedly: by cron, by `composelock poll` as a long-running process, or by `composelock webhook` on push. Use cron if you do not want a supervised background process; use `poll` or `webhook` under systemd, Docker or a similar supervisor, as the [container example](#run-as-a-container) does.

Cron setup and the webhook server are covered in [docs/running.md](docs/running.md).

## Development

```sh
make build    # produces bin/composelock
make test     # go test -race -shuffle=on ./...
```

Every make target, the smoke tests, versioning and releases are covered in [docs/development.md](docs/development.md).

## AI Development Disclosure

This project was developed with the assistance of AI/LLM tools as part of the development workflow.

**Human Oversight:** All AI-generated code was manually reviewed, tested, modified, and optimized by the project maintainers. The final implementation reflects human judgment and understanding of the project requirements.
