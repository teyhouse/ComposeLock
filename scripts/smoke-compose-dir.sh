#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT="composelock-smoke-dir"
STACKS=("$PROJECT" "$PROJECT-db" "$PROJECT-c")
IMAGE="${SMOKE_IMAGE:-alpine:3.22}"

if [ -n "${SMOKE_WORKDIR:-}" ]; then
	WORK="$SMOKE_WORKDIR"
	mkdir -p "$WORK"
	if [ -n "$(ls -A "$WORK")" ]; then
		echo "SMOKE_WORKDIR $WORK is not empty" >&2
		exit 2
	fi
	CREATED=0
else
	WORK="$(mktemp -d "${TMPDIR:-/tmp}/composelock-smoke-dir.XXXXXX")"
	CREATED=1
fi

BIN="$WORK/composelock"
CFG="$WORK/composelock.json"
SEED="$WORK/seed"
REPO="$WORK/repo"
SUCCESS=0

remove_stacks() {
	for p in "${STACKS[@]}"; do
		docker ps -aq --filter "label=com.docker.compose.project=$p" | xargs -r docker rm -f >/dev/null
		docker network rm "${p}_default" >/dev/null 2>&1 || true
	done
}

cleanup() {
	remove_stacks
	if [ "$SUCCESS" = "1" ] && [ "$CREATED" = "1" ] && [ "${SMOKE_KEEP:-0}" != "1" ]; then
		rm -rf "$WORK"
	else
		echo "work directory kept: $WORK" >&2
	fi
}
trap cleanup EXIT

publish() {
	git -C "$SEED" add -A
	git -C "$SEED" commit -qm "$1"
	git -C "$SEED" push -q origin main
}

# write_stack_file <path relative to deployment/> <command JSON array>
write_stack_file() {
	local rel=$1 command=$2 name
	name=$(basename "$rel" | sed -E 's/\.ya?ml$//')
	mkdir -p "$(dirname "$SEED/deployment/$rel")"
	cat >"$SEED/deployment/$rel" <<EOF
services:
  $name:
    image: $IMAGE
    command: $command
    healthcheck:
      test: ["CMD", "true"]
      interval: 2s
      timeout: 2s
      retries: 2
EOF
}

write_config() {
	cat >"$CFG" <<EOF
{
  "repo_path": "$REPO",
  "remote": "origin",
  "branch": "main",
  "compose_dir": "$REPO/deployment",
  "project_name": "$PROJECT",
  "state_file": "$WORK/state.json",
  "retry_attempts": 1,
  "retry_delay_seconds": 1,
  "health_watch_seconds": 10,
  "health_poll_interval_seconds": 2,
  "health_unhealthy_streak": 2,
  "health_restart_tolerance": 0,
  "log_format": "json"
}
EOF
}

run_step() {
	local name=$1 want=$2
	shift 2
	local log="$WORK/logs/$name.log" got=0
	"$BIN" --config "$CFG" "$@" >"$log" 2>&1 || got=$?
	if [ "$got" -ne "$want" ]; then
		echo "FAIL $name: exit $got, want $want (log: $log)" >&2
		tail -n 5 "$log" >&2
		exit 1
	fi
	echo "ok   $name (exit $got)"
	sleep 2
}

expect_log() {
	local name=$1 pattern=$2
	if ! grep -q -- "$pattern" "$WORK/logs/$name.log"; then
		echo "FAIL $name: log does not contain '$pattern'" >&2
		exit 1
	fi
}

# container_id <project> prints the (single) container ID for that
# compose project, or nothing if it has none running.
container_id() {
	docker ps -q --filter "label=com.docker.compose.project=$1"
}

# expect_unchanged <label> <project> <expected id>
expect_unchanged() {
	local label=$1 project=$2 want=$3 got
	got=$(container_id "$project")
	if [ "$got" != "$want" ]; then
		echo "FAIL $label: $project container id changed ($want -> $got), expected untouched" >&2
		exit 1
	fi
}

# expect_running <label> <project>
expect_running() {
	local label=$1 project=$2
	if [ -z "$(container_id "$project")" ]; then
		echo "FAIL $label: expected $project to have a running container" >&2
		exit 1
	fi
}

# expect_absent <label> <project>
expect_absent() {
	local label=$1 project=$2
	if [ -n "$(container_id "$project")" ]; then
		echo "FAIL $label: expected $project to have no running container" >&2
		exit 1
	fi
}

# expect_container_count <label> <project> <count>
expect_container_count() {
	local label=$1 project=$2 want=$3 got
	got=$(docker ps -q --filter "label=com.docker.compose.project=$project" | wc -l | tr -d ' ')
	if [ "$got" != "$want" ]; then
		echo "FAIL $label: $project has $got running containers, want $want" >&2
		exit 1
	fi
}

# expect_state_at_head <label>: last_healthy_commit must match the commit
# that is actually checked out. When it lags behind, a later failure rolls
# back further than the change that broke.
expect_state_at_head() {
	local label=$1 head got
	head=$(git -C "$REPO" rev-parse HEAD)
	got=$(sed -n 's/.*"last_healthy_commit": "\([^"]*\)".*/\1/p' "$WORK/state.json")
	if [ "$got" != "$head" ]; then
		echo "FAIL $label: last_healthy_commit $got, want checked-out HEAD $head" >&2
		exit 1
	fi
}

remove_stacks
mkdir -p "$WORK/logs"
go -C "$ROOT" build -o "$BIN" ./cmd/composelock

git init -q --bare -b main "$WORK/origin.git"
git init -q -b main "$SEED"
git -C "$SEED" config user.name composelock-smoke
git -C "$SEED" config user.email smoke@composelock.invalid
git -C "$SEED" config core.autocrlf false
git -C "$SEED" remote add origin "$WORK/origin.git"
echo "# composelock compose_dir smoke stack" >"$SEED/README.md"
publish "initial"
git clone -q "$WORK/origin.git" "$REPO"
git -C "$REPO" config core.autocrlf false
write_config

# v1: three independent stacks: root ("$PROJECT"), db/ ("$PROJECT-db"),
# c/ ("$PROJECT-c"). c/ is never touched again after this — it's the
# witness proving unrelated stacks are left alone.
write_stack_file "service-a.yaml" '["sleep", "infinity"]'
write_stack_file "db/docker-compose.yaml" '["sleep", "infinity"]'
write_stack_file "c/docker-compose.yaml" '["sleep", "infinity"]'
publish "v1: three healthy stacks"
run_step 01-deploy-all 0 sync
expect_log 01-deploy-all '"applied":true'
expect_running 01-deploy-all "$PROJECT"
expect_running 01-deploy-all "$PROJECT-db"
expect_running 01-deploy-all "$PROJECT-c"
expect_state_at_head 01-deploy-all

ROOT_ID=$(container_id "$PROJECT")
DB_ID=$(container_id "$PROJECT-db")
C_ID=$(container_id "$PROJECT-c")

# v2: change only db/ — root and c must never be recreated.
write_stack_file "db/docker-compose.yaml" '["sleep", "999999"]'
publish "v2: db stack changes alone"
run_step 02-partial-change-db-only 0 sync
expect_log 02-partial-change-db-only '"applied":true'
expect_unchanged 02-partial-change-db-only "$PROJECT" "$ROOT_ID"
expect_unchanged 02-partial-change-db-only "$PROJECT-c" "$C_ID"
DB_ID=$(container_id "$PROJECT-db")
if [ -z "$DB_ID" ]; then
	echo "FAIL 02-partial-change-db-only: db stack has no running container" >&2
	exit 1
fi

# v3: root stack starts crashing — only root (this cycle's only changed
# stack) reverts. db and c, untouched this cycle, must never be recreated,
# proving atomic-per-cycle revert scope rather than whole-deployment revert.
write_stack_file "service-a.yaml" '["sh", "-c", "sleep 3; exit 1"]'
publish "v3: root stack crashes"
run_step 03-root-crash-reverts 1 sync
expect_log 03-root-crash-reverts '"reverted":true'
expect_unchanged 03-root-crash-reverts "$PROJECT-db" "$DB_ID"
expect_unchanged 03-root-crash-reverts "$PROJECT-c" "$C_ID"
expect_running 03-root-crash-reverts "$PROJECT"
ROOT_ID=$(container_id "$PROJECT")

# v3.5: fix root back to healthy. Needed before v4/v5 below: reverting
# only moves the local checkout, never origin/main, so until a new commit
# actually fixes service-a.yaml, every subsequent commit's cumulative diff
# still carries v3's crash change and would keep re-triggering it. This
# restores byte-identical content to what's already checked out (the
# revert target), so git reports no diff for it and this sync is a quiet
# no-op skip — that's expected: it means the crash content is genuinely
# gone from the cumulative diff from here on, not that anything reapplied.
write_stack_file "service-a.yaml" '["sleep", "infinity"]'
publish "v3.5: root stack fixed"
run_step 03b-root-fixed 0 sync
expect_unchanged 03b-root-fixed "$PROJECT" "$ROOT_ID"
expect_unchanged 03b-root-fixed "$PROJECT-db" "$DB_ID"
expect_unchanged 03b-root-fixed "$PROJECT-c" "$C_ID"

# v4: an invalid compose file anywhere in compose_dir fails the whole
# cycle loudly, before touching Docker at all — none of the three stacks'
# containers change.
mkdir -p "$SEED/deployment/broken"
cat >"$SEED/deployment/broken/broken.yaml" <<'EOF'
services: not-a-valid-services-mapping
EOF
publish "v4: broken compose file added"
run_step 04-invalid-file-fails-loudly 1 sync
expect_log 04-invalid-file-fails-loudly 'broken.yaml'
expect_unchanged 04-invalid-file-fails-loudly "$PROJECT" "$ROOT_ID"
expect_unchanged 04-invalid-file-fails-loudly "$PROJECT-db" "$DB_ID"
expect_unchanged 04-invalid-file-fails-loudly "$PROJECT-c" "$C_ID"

# v5: removing the broken file recovers; no stack's files actually
# changed, so this is a quiet no-op.
rm -rf "$SEED/deployment/broken"
publish "v5: broken compose file removed"
run_step 05-recovers-after-fix 0 sync
expect_unchanged 05-recovers-after-fix "$PROJECT" "$ROOT_ID"
expect_unchanged 05-recovers-after-fix "$PROJECT-db" "$DB_ID"
expect_unchanged 05-recovers-after-fix "$PROJECT-c" "$C_ID"

# v6: removing the c/ subdirectory entirely tears down its stack via
# Compose.Down, while root and db (never touched this cycle) stay exactly
# as they are.
rm -rf "$SEED/deployment/c"
publish "v6: c stack directory removed"
run_step 06-removed-stack-torn-down 0 sync
expect_absent 06-removed-stack-torn-down "$PROJECT-c"
expect_unchanged 06-removed-stack-torn-down "$PROJECT" "$ROOT_ID"
expect_unchanged 06-removed-stack-torn-down "$PROJECT-db" "$DB_ID"
# Nothing was applied this cycle, but the checkout moved, so the commit has
# to be recorded as the new rollback target.
expect_state_at_head 06-removed-stack-torn-down

# v7: a YAML file that is not a compose file, sitting next to one, is
# skipped by discovery instead of failing the whole deployment.
cat >"$SEED/deployment/db/prometheus.yml" <<'EOF'
global:
  scrape_interval: 15s
scrape_configs:
  - job_name: smoke
EOF
publish "v7: non-compose yaml next to a compose file"
run_step 07-non-compose-yaml-ignored 0 sync
if grep -q "invalid compose file" "$WORK/logs/07-non-compose-yaml-ignored.log"; then
	echo "FAIL 07-non-compose-yaml-ignored: prometheus.yml was treated as a compose file" >&2
	exit 1
fi
expect_running 07-non-compose-yaml-ignored "$PROJECT-db"
expect_unchanged 07-non-compose-yaml-ignored "$PROJECT" "$ROOT_ID"
expect_state_at_head 07-non-compose-yaml-ignored
DB_ID=$(container_id "$PROJECT-db")

# v8: a commit that ADDS a compose file to an existing stack and then fails
# its health watch. The revert has to load the root stack from the rollback
# target's own file list, where the added file does not exist, and end up
# with only the surviving service running. Loading the failing commit's file
# list here would fail the revert itself and go DEGRADED (exit 3).
write_stack_file "service-b.yaml" '["sh", "-c", "sleep 3; exit 1"]'
publish "v8: added root stack file crashes"
run_step 08-added-file-crash-reverts 1 sync
expect_log 08-added-file-crash-reverts '"reverted":true'
expect_log 08-added-file-crash-reverts '"degraded":false'
expect_container_count 08-added-file-crash-reverts "$PROJECT" 1
expect_unchanged 08-added-file-crash-reverts "$PROJECT-db" "$DB_ID"

SUCCESS=1
echo "compose_dir smoke test passed"
