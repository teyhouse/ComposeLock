# Configuration

[Back to README](../README.md)

```sh
composelock --init --config composelock.json --with-state
```

This writes a default `composelock.json` and an empty `state.json`. Config path resolution order: the `--config` flag, then `$COMPOSELOCK_CONFIG`, then `./composelock.json`.

`repo_path`, `project_name`, and one of `compose_file`/`compose_dir` are required. `remote` and `branch` allow only letters, digits, `.`, `_`, `/` and `-`, and may not start with `-` or contain `..`, since both are passed straight to `git`. A config whose `schema_version` is newer than the binary understands is refused rather than loaded with its new fields ignored, and unknown fields are warned about by full dotted path, so `webhook.secrt` is reported instead of quietly leaving the webhook unauthenticated. Everything else has a sane default:

| Field                          | Default              | Meaning                                        |
|---------------------------------|-----------------------|-------------------------------------------------|
| `remote`                        | `origin`              | Git remote to fetch                             |
| `branch`                        | `main`                | Branch to track                                 |
| `compose_dir`                   | (disabled)            | Directory of compose files; overrides `compose_file` when set, see [Multiple compose files](compose-dir.md) |
| `ssh_key`                       | (system agent)        | SSH key for Git, if not using the agent         |
| `state_file`                    | `./state.json`        | Where reconcile state is stored                 |
| `retry_attempts`                | `3`                   | Git fetch retry attempts                        |
| `retry_delay_seconds`           | `20`                  | Delay between Git retries                       |
| `docker_timeout_seconds`        | `60`                  | Timeout for Docker calls other than `up`; `0` disables it |
| `docker_up_timeout_seconds`     | `1800`                | Timeout for `compose up`, which may pull or build; `0` disables it |
| `poll_interval_seconds`         | `0`                   | Used only by `composelock poll`; `0` disables it |
| `monitor_interval_seconds`      | `60`                  | How often `poll`/`webhook` check the deployed stacks for [health alerts](notifications.md#health-alerts); `0` disables it |
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

To deploy a whole directory of compose files instead of a single `compose_file`, see [Multiple compose files](compose-dir.md).
