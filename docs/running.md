# Keeping it running

[Back to README](../README.md)

`composelock sync` is a single self-contained reconcile, meant to be invoked repeatedly: by cron, by `composelock poll` as a long-running process, or by `composelock webhook` on push. Use cron if you do not want a supervised background process; use `poll` or `webhook` under systemd, Docker or a similar supervisor, as the [container example](../README.md#run-as-a-container) does. Only `poll` and `webhook` send [health alerts](notifications.md#health-alerts) between deploys, since cron has no process running in between.

## Cron

Running every 5 minutes:

```
*/5 * * * * /usr/local/bin/composelock --config /etc/composelock/composelock.json sync >> /var/log/composelock.log 2>&1
```

- Use absolute paths for the binary, `--config`, and every path inside the config. Cron runs with a minimal environment and an unpredictable working directory.
- The cron user needs access to the Docker socket, usually via the `docker` group.
- Logs go to stdout; redirecting to a file is the simplest record. Set `log_format` to `text` to read it without a JSON processor.
- Exit codes drive alerting: a wrapper checking `$?`, or cron's `MAILTO`, covers exit 1 and 3. Discord notifications cover the same cases without parsing output.
- Never run `poll` from cron. It is a long-running loop and will pile up one process per tick.

## Webhook

`composelock webhook` serves `webhook.path` on `webhook.listen` and reconciles on push. Set `webhook.secret` to the GitHub webhook secret so requests are authenticated with `X-Hub-Signature-256`. An empty secret lets anything that reaches the port trigger a deploy, so it is only accepted on a loopback `webhook.listen`; a non-loopback address without a secret is rejected at startup. To expose it publicly, either set a secret or keep the listener on loopback behind a reverse proxy. A `ping` is answered without deploying, non-push events and pushes to other branches are ignored, and pushes arriving during a reconcile coalesce into one follow-up run.

## Heartbeat

Discord only reports what ComposeLock does, so a process that crashed, was never restarted, or hangs inside a reconcile stays silent. Set `heartbeat_url` to a dead-man's-switch check, such as a [healthchecks.io](https://healthchecks.io) ping URL or an Uptime Kuma push monitor, and ComposeLock sends a `GET` to it after every completed reconcile and, in `poll` and `webhook` mode, after every [health monitor](notifications.md#health-alerts) check. When the pings stop, that service alerts you.

- Every run counts as alive, whether it deployed, found nothing to do, or failed: failures are reported through Discord. `check` (dry run) never pings.
- A monitor check skipped because a reconcile is still running does not ping, so a reconcile that hangs stops the heartbeat instead of hiding behind it.
- Set the check's period to the ping interval (`monitor_interval_seconds`, or the cron or `poll_interval_seconds` schedule when the monitor is off) and its grace time above the longest deploy you expect (`health_watch_seconds` plus image pulls), since a deploy in progress pauses the pings.
- In `webhook` mode with `monitor_interval_seconds` set to `0`, pings arrive only on pushes, so the check needs a period longer than your quietest week.
- The URL identifies your check, so it is never logged and has no command-line flag.

`composelock webhook` also answers `GET /healthz` with `200 ok` on `webhook.listen`, for a reverse proxy or an external HTTP monitor. It shows that the process and its listener are up, not that reconciles are progressing; the heartbeat covers that. The image ships without `curl` or `wget`, so it has no built-in `HEALTHCHECK`.

## Pausing deployments

While you work on the host, hold ComposeLock with `composelock pause`, and lift the hold with `composelock resume`:

```sh
composelock --config /srv/composelock.json pause -for 2h -reason "NAS upgrade"
composelock --config /srv/composelock.json resume
```

A paused run still fetches and logs the commit that is waiting, but applies, reverts and recovers nothing, and the [health monitor](notifications.md#health-alerts) sends no alerts, so containers you stop on purpose do not page anyone. `-for` ends the pause on its own; without it the pause lasts until `resume`. `status` shows the pause and its reason. The heartbeat keeps pinging, since ComposeLock itself is still running. The pause lives in the state file, so it survives restarts and applies to cron, `poll` and `webhook` alike. If a reconcile is running, `pause` waits for it to finish rather than interrupting it.

## Deploy window

To let pushed changes land only at a quiet time, set `deploy_window` in the config:

```json
{
  "deploy_window": "02:00-05:00"
}
```

Outside the window, a run that finds a new commit fetches it, logs `outside the deploy window, not applying` with the waiting commit and the next opening time, and exits 0 without touching anything. The first run inside the window deploys it through the usual pre-flight gate and health watch. `status` shows the window and whether it is open.

- The format is `HH:MM-HH:MM` in 24-hour time. The start is inclusive and the end exclusive, so `02:00-05:00` allows 02:00 up to 04:59. A window may cross midnight: `22:00-06:00` covers the night.
- Times are local to the ComposeLock process. The container image runs in UTC unless you set `TZ`, for example `-e TZ=Europe/Berlin`. The time zone database is built into the binary, so `TZ` works even though the image ships no zoneinfo files.
- The window only holds new commits. A deploy that is already running finishes, and crash recovery or an interrupted revert resumes at any time, since those restore a known-good state rather than bring in a change. The very first deploy of a fresh install is not held either.
- The window is optional: without `deploy_window` (or with an empty value) commits deploy whenever they arrive, exactly as before, so existing configs need no change.
- To deploy right away outside the window, override it for a single run: `composelock --config /srv/composelock.json -deploy-window "" sync`.
- The window applies to cron, `poll` and `webhook` alike. With cron, schedule at least one run inside the window; with `poll`, the first tick after it opens deploys. With `webhook`, a push held outside the window schedules one reconcile for the moment the window opens, so it deploys without another push.

## Deployment history

`composelock status` lists the last 20 runs that did something: deploys, reverts, DEGRADED, and failures, each with its time, result and commit, the rollback target for a revert, and a shortened error. Identical consecutive entries, such as a git fetch failing every tick during a network outage, are folded into one line with a repeat count. Runs that found nothing to do are not recorded. The history is stored in the state file.

State files from earlier versions need no changes: the pause and history fields are simply absent until first used.
