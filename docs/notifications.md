# Notifications

[Back to README](../README.md)

Notifications go to Discord, to a [generic webhook](#generic-webhook), or to both. Set `discord_webhook` to post a colour-coded embed for every outcome that matters: a successful deploy (green), a revert that recovered the stack (orange), and a failure or DEGRADED state (red). Repeated identical notifications are suppressed for an hour per key, so a stack failing every poll tick does not flood the channel, and a notification that failed to send never consumes that hour. The key names the event and its commit, and a pre-flight DEGRADED (which has no failing commit of its own) is keyed by its rollback target, so two of them do not silence each other.

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

## Generic webhook

To send notifications anywhere other than Discord (n8n, Node-RED, Home Assistant, a chat bridge, your own script), set `notify_webhook`. It receives the same events as Discord, including [health alerts](#health-alerts), and works alongside `discord_webhook` or on its own:

```json
{
  "notify_webhook": {
    "url": "https://automation.example.com/webhook/composelock",
    "headers": {
      "Authorization": "Bearer 0123456789abcdef"
    }
  }
}
```

### Request

Every notification is one `POST` to `notify_webhook.url` with `Content-Type: application/json` and `User-Agent: composelock`, plus any `headers` you configure (for tokens or routing). `POST` rather than `GET`, because the event travels in the body: a query string would end up in proxy and access logs.

| Field            | Always present | Meaning                                                                                                         |
|------------------|----------------|-----------------------------------------------------------------------------------------------------------------|
| `schema_version` | yes            | Payload format version, currently `1`. It changes only when a field is removed or changes meaning                |
| `source`         | yes            | Always `composelock`                                                                                            |
| `status`         | yes            | `success` (deployed), `recovered` (reverted, healthy again) or `failure` (failed, DEGRADED, or a health alert)   |
| `title`          | yes            | Short summary, e.g. `ComposeLock: deployed successfully` or `Stack unhealthy: my-stack`                          |
| `message`        | no             | Commit subject and author; for a revert, the failed commit and the rollback target                               |
| `commit`         | no             | Full hash of the commit the event is about                                                                      |
| `commit_url`     | no             | Link to that commit, when a [repository web URL](configuration.md) is known                                     |
| `compare_url`    | no             | Link comparing it with the previously deployed commit or the rollback target                                    |
| `fields`         | yes            | The same details as the Discord embed, as plain text keyed by name: `Branch`, `Services`, `Updated`, `Health Watch`, `Duration`, `Error`, `Stack`, `Problem` and so on, depending on the event |
| `timestamp`      | yes            | When the notification was sent, in UTC (RFC 3339)                                                               |

A successful deploy:

```json
{
  "schema_version": 1,
  "source": "composelock",
  "status": "success",
  "title": "ComposeLock: deployed successfully",
  "message": "bump vaultwarden to 1.34.1\nteyhouse",
  "commit": "ecd4ea8c12c432380a53370eab59e33c4344617b",
  "commit_url": "https://github.com/teyhouse/homelab/commit/ecd4ea8c12c432380a53370eab59e33c4344617b",
  "fields": {
    "Branch": "main",
    "Commit": "ecd4ea8",
    "Duration": "5m36s",
    "Health Watch": "5m0s (success)",
    "Services": "pihole, traefik, vaultwarden",
    "Updated": "vaultwarden"
  },
  "timestamp": "2026-09-23T17:34:13.025628878Z"
}
```

A failed deploy that was reverted:

```json
{
  "schema_version": 1,
  "source": "composelock",
  "status": "recovered",
  "title": "ComposeLock: reverted, healthy again",
  "message": "Failed: 9aa26f7 switch traefik to v3 config (teyhouse)\nRollback target: ecd4ea8 bump vaultwarden to 1.34.1 (teyhouse)",
  "commit": "9aa26f721689b6aeba1ebb25d7101dc5102cd041",
  "commit_url": "https://github.com/teyhouse/homelab/commit/9aa26f721689b6aeba1ebb25d7101dc5102cd041",
  "compare_url": "https://github.com/teyhouse/homelab/compare/ecd4ea8c12c432380a53370eab59e33c4344617b...9aa26f721689b6aeba1ebb25d7101dc5102cd041",
  "fields": {
    "Branch": "main",
    "Commit": "9aa26f7",
    "Duration": "5m21s",
    "Error": "applied 9aa26f7 failed health watch (my-stack: container exited: traefik (exit code 1)), reverted to ecd4ea8: healthy again",
    "Health Watch": "5m0s (recovered)",
    "Services": "traefik"
  },
  "timestamp": "2026-09-23T17:34:43.314282091Z"
}
```

Branch on `status` for routing and use `title` and `message` as the human-readable text; `fields` is for display, and its keys follow what the Discord embed shows.

### Delivery

- Any `2xx` response counts as delivered. A `429` is retried once after its `Retry-After` (at most 15 seconds); any other status, or no answer within 10 seconds, is logged and the notification is dropped.
- Discord and the webhook are throttled separately: when one of them fails, only that one is retried the next time the event repeats, so the other does not receive duplicates.
- The URL and header values can carry credentials, so they are never logged and have no command-line flag.

### Trying it out

A minimal receiver that prints every payload, useful for building your own integration:

```python
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        print(json.dumps(json.loads(body), indent=2))
        self.send_response(204)
        self.end_headers()

HTTPServer(("127.0.0.1", 9000), Handler).serve_forever()
```

Point `notify_webhook.url` at `http://127.0.0.1:9000/` and run `composelock sync`.
