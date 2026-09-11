#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT="composelock-smoke"
IMAGE="${SMOKE_IMAGE:-alpine:3.22}"
WEBHOOK="${COMPOSELOCK_SMOKE_DISCORD_WEBHOOK:-}"

if [ -n "${SMOKE_WORKDIR:-}" ]; then
	WORK="$SMOKE_WORKDIR"
	mkdir -p "$WORK"
	if [ -n "$(ls -A "$WORK")" ]; then
		echo "SMOKE_WORKDIR $WORK is not empty" >&2
		exit 2
	fi
	CREATED=0
else
	WORK="$(mktemp -d "${TMPDIR:-/tmp}/composelock-smoke.XXXXXX")"
	CREATED=1
fi

BIN="$WORK/composelock"
CFG="$WORK/composelock.json"
SEED="$WORK/seed"
REPO="$WORK/repo"
SUCCESS=0

remove_stack() {
	docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" | xargs -r docker rm -f >/dev/null
	docker network rm "${PROJECT}_default" >/dev/null 2>&1 || true
}

cleanup() {
	remove_stack
	if [ "$SUCCESS" = "1" ] && [ "$CREATED" = "1" ] && [ "${SMOKE_KEEP:-0}" != "1" ]; then
		rm -rf "$WORK"
	else
		echo "work directory kept: $WORK (its composelock.json contains the webhook URL)" >&2
	fi
}
trap cleanup EXIT

publish() {
	git -C "$SEED" add -A
	git -C "$SEED" commit -qm "$1"
	git -C "$SEED" push -q origin main
}

write_compose() {
	local web_command=$1 web_extra=${2:-}
	cat >"$SEED/docker-compose.yml" <<EOF
services:
  web:
    image: $IMAGE
    command: $web_command
    healthcheck:
      test: ["CMD", "true"]
      interval: 2s
      timeout: 2s
      retries: 2
$web_extra
  migrate:
    image: $IMAGE
    command: ["true"]
EOF
}

write_config() {
	(
		umask 077
		cat >"$CFG" <<EOF
{
  "repo_path": "$REPO",
  "remote": "origin",
  "branch": "main",
  "compose_file": "$REPO/docker-compose.yml",
  "project_name": "$PROJECT",
  "state_file": "$WORK/state.json",
  "retry_attempts": 1,
  "retry_delay_seconds": 1,
  "health_watch_seconds": 10,
  "health_poll_interval_seconds": 2,
  "health_unhealthy_streak": 2,
  "health_restart_tolerance": 0,
  "discord_webhook": "$WEBHOOK",
  "log_format": "json"
}
EOF
	)
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

if [ -z "$WEBHOOK" ]; then
	echo "COMPOSELOCK_SMOKE_DISCORD_WEBHOOK is not set: running without Discord notifications"
fi

remove_stack
mkdir -p "$WORK/logs"
go -C "$ROOT" build -o "$BIN" ./cmd/composelock

git init -q --bare -b main "$WORK/origin.git"
git init -q -b main "$SEED"
git -C "$SEED" config user.name composelock-smoke
git -C "$SEED" config user.email smoke@composelock.invalid
git -C "$SEED" config core.autocrlf false
git -C "$SEED" remote add origin "$WORK/origin.git"
echo "# composelock smoke stack" >"$SEED/README.md"
publish "initial"
git clone -q "$WORK/origin.git" "$REPO"
git -C "$REPO" config core.autocrlf false
write_config

write_compose '["sleep", "infinity"]'
publish "v1: healthy stack"
run_step 01-deploy 0 sync
expect_log 01-deploy '"applied":true'

write_compose '["sh", "-c", "sleep 3; exit 1"]'
publish "v2: web crashes"
run_step 02-crash-reverts 1 sync
expect_log 02-crash-reverts '"reverted":true'

run_step 03-known-bad-skipped 0 sync
expect_log 03-known-bad-skipped 'skipping known-bad commit'

write_compose '["sleep", "infinity"]' '    env_file: app.env'
publish "v3: requires app.env"
run_step 04-env-file-missing 1 sync
expect_log 04-env-file-missing 'app.env'

touch "$REPO/app.env"
run_step 05-env-file-fixed 0 sync
expect_log 05-env-file-fixed '"applied":true'

run_step 06-git-sync-fails 1 --remote does-not-exist sync
expect_log 06-git-sync-fails 'git sync failed'

write_compose '["sh", "-c", "sleep 3; exit 1"]'
publish "v4: web crashes, no rollback target"
run_step 07-degraded 3 --state-file "$WORK/fresh-state.json" sync
expect_log 07-degraded '"degraded":true'
run_step 08-degraded-refuses 3 --state-file "$WORK/fresh-state.json" sync
expect_log 08-degraded-refuses 'refusing to reconcile'

if grep -h 'discord notify:' "$WORK"/logs/*.log >&2; then
	echo "FAIL: Discord rejected or never received a notification" >&2
	exit 1
fi
if [ -n "$WEBHOOK" ] && grep -qF "${WEBHOOK##*/}" "$WORK"/logs/*.log; then
	echo "FAIL: webhook token found in logs" >&2
	exit 1
fi

SUCCESS=1
echo "smoke test passed"
if [ -n "$WEBHOOK" ]; then
	echo "expect 6 Discord notifications: deployed, reverted, env_file failure, deployed, git sync failed, DEGRADED"
fi
