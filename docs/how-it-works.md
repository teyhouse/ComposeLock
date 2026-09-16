# How it works

[Back to README](../README.md)

## What counts as a change

A relative `compose_file` resolves against `repo_path`, like `compose_dir` does, so it means the same file wherever ComposeLock is started from.

A relative `repo_path` (the `"."` that `--init` scaffolds) resolves against the working directory ComposeLock starts in, once, at config load. Every path it compares afterwards is absolute, so change detection means the same thing whether it is configured relative or absolute. From cron or systemd an absolute `repo_path` is still clearer, since a relative one silently follows whatever `WorkingDirectory` the unit happens to set.

A commit only deploys when it touches something the deployment depends on. In `compose_file` mode that is the compose file plus every input reachable from it: `include:` fragments and their nested includes, `extends.file` targets, each `env_file`, the implicit `.env` beside the compose file and beside each included project, every service build context and Dockerfile, and any `config`/`secret` sourced from a file. A tracked `.env`, Dockerfile or fragment therefore redeploys; a docs-only commit does not. Services declaring `build:` are rebuilt on every apply rather than only when their image is missing, so a Dockerfile-only commit reaches the running container instead of passing its watch against the previous image.

Two cases apply rather than skip, since a wrong skip is silent and a wrong apply is not: a project that fails to load at all, and an input set that cannot be enumerated statically, such as an `include:` path built from a variable (`${STACK_DIR}/compose.yaml`). Remote `include:` references (`https://`, `git@`, `oci://`) are ignored, since they can never be a path in this repository. `compose_dir` mode uses a broader and cheaper rule, see [Multiple compose files](compose-dir.md).

An irrelevant commit still moves the checkout forward, so `HEAD` never falls behind the branch, but it is recorded in `last_checkout_commit`, not `last_healthy_commit`: the rollback target is only ever a commit that was applied and passed a watch.

## The pre-flight gate

Before applying anything, ComposeLock snapshots the deployed stack. If it is unhealthy the new commit is not applied at all, because a failure on top of a broken stack cannot be attributed. The last healthy commit is re-applied and watched instead, and three blocked runs in a row end DEGRADED rather than repeating a full watch window forever. A commit that already failed is skipped the same way, so a bad commit fails once rather than every poll tick, until a new commit lands or you pass `--force`. The last several failed commits are remembered, so two bad commits do not take turns being retried.

The checkout only moves once the gate has passed, so a gate failure (Docker unreachable) leaves the worktree untouched for the next run. Discovery failing at the *current* checkout is deliberately not a gate failure: a broken compose file already checked out would otherwise wedge the deployment there forever, since the commit repairing it could never be checked out. Such a run warns, falls back to `last_healthy_stacks` for the gate, and carries on, so `git checkout --detach --force` repairs the worktree.

Checks that need the new tree (discovering its stacks, loading each project, verifying every `env_file` exists) run right after the checkout and before Compose is touched. If one fails, the worktree is restored and the commit recorded as known-bad. Push a new commit or run `composelock sync --force` to retry, which is what you want after fixing an `env_file` that lives on the host rather than in the repository.

`git checkout` runs `--detach --force`: the worktree is a deployment artifact and the branch is the only source of truth, so local edits under `repo_path` are discarded rather than carried into the next deploy.

## The health watch

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

## Revert and DEGRADED

If the watch fails, ComposeLock checks out the last healthy commit, re-applies it, and watches that too. The revert loads every stack and verifies its `env_file` paths before the first `compose up`, so a rollback that cannot succeed fails cleanly instead of leaving half the stacks at the target and half at the failed commit. A successful revert exits 1: the deploy failed, but the stack is serving again. If the revert fails its own watch, or there is no healthy commit to fall back to, the run ends DEGRADED and exits 3.

Infrastructure failures are treated differently from bad commits. A daemon restart, a timeout or a connection refused during `compose up` neither marks the commit bad nor reverts into the same broken daemon: the apply is left pending for the next run, which is what would have recovered it anyway. The same holds during a revert, and a revert interrupted before its watch finishes, by a shutdown or a blip, is recorded as pending and resumed rather than forgotten.

## Crash recovery

State (last healthy commit and the stacks it deployed, last failed commit, last checkout, pending commit and the stacks it was applying, last result) lives in a JSON file, so a crash mid-deploy is recoverable: an interrupted deploy is re-applied and watched again, an interrupted revert is finished, both limited to the stacks that run was touching. Every recovery run fetches the branch first, so a commit pushed to repair a failing deploy is applied instead of the pending one rather than staying invisible.

Recovery gives up and goes DEGRADED rather than looping, after three attempts or once the pending commit has been unresolved for longer than the recovery budget, whichever comes first. The budget is three times the watch window plus 15 minutes of grace per attempt, so an hour at the defaults. Only a real apply or revert spends an attempt: a Docker blip or an unreadable `compose_dir` returns without consuming one. Going DEGRADED clears the pending block and records the in-flight commit in `last_attempt_commit` and `failed_commits` rather than replaying it, so `composelock sync --force` resets the counter and takes the branch as it stands now.

Teardown and checkout restore run to completion through SIGINT or SIGTERM, because a half-removed stack is worse than a slow exit. Stacks are torn down concurrently, up to eight at a time, so shutdown during a teardown is bounded by `docker_timeout_seconds` per stack (capped at two minutes each) plus 30 seconds for the checkout restore, rather than by the sum over every stack. `sync` and `poll` run the reconcile to the end; `webhook` stops accepting triggers, then waits for the in-flight run.

Only one reconcile touches a deployment at a time. A second process finds the state file lock held and exits 0, which makes it safe to run cron and a webhook server against the same config.
