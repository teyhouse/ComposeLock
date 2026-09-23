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
