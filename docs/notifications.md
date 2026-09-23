# Notifications

[Back to README](../README.md)

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

Under the title, the embed shows the commit subject, the author and a compare link to the previously deployed commit, and says how many commits landed when there was more than one. A revert or DEGRADED embed names both the failed commit and the rollback target. The `Commit` field and the compare link point at the repository's web page, which is derived from the remote (`git@github.com:owner/repo.git` and `https://github.com/owner/repo.git` both become `https://github.com/owner/repo`, with any credentials in the remote URL dropped). Set `repo_url` when the web address differs from the remote, for example a self-hosted Gitea reached over SSH on another host name. A remote that is a local path gets no links.

Compose leaves a service alone when its configuration did not change, so `Updated` narrows `Services` to what actually moved: a one-service bump lists seven names under `Services` and one under `Updated`. It reads `none` when a compose file changed without changing any container, and is omitted entirely when `health_watch_seconds` is `0`, since there is nothing to compare against. The same list is logged as `updated_services`.

## Health alerts

In `poll` and `webhook` mode, ComposeLock also checks the deployed stacks every `monitor_interval_seconds` (default 60) between deploys, and posts a red "Stack unhealthy" embed to the same `discord_webhook` when a stack stays unhealthy for `health_unhealthy_streak` consecutive checks (default 3). A stack counts as unhealthy when a container is not running or its healthcheck reports `unhealthy`, when a container restarted since the previous check, or when a service that was running has no containers any more. There is one alert per incident: nothing more is sent while the stack stays broken, and nothing is sent when it recovers. It only alerts and never touches the stack. Checks are skipped while a reconcile is running or a deploy is pending, since those send their own notifications.

A long-running service that was stopped cleanly with exit code 0 looks like a finished one-shot container and is not flagged.
