# Development

[Back to README](../README.md)

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

## Smoke tests

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
