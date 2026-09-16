# Multiple compose files (`compose_dir`)

[Back to README](../README.md)

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
