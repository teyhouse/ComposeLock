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
REC_STATE="$WORK/recovery-state.json"
REL_CFG="$WORK/composelock-relative.json"
REL_STATE="$WORK/relative-state.json"
LOCK_STATE="$WORK/lock-state.json"
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
	local out=${1:-$CFG} compose_file=${2:-$REPO/docker-compose.yml} state=${3:-$WORK/state.json}
	(
		umask 077
		cat >"$out" <<EOF
{
  "repo_path": "$REPO",
  "remote": "origin",
  "branch": "main",
  "compose_file": "$compose_file",
  "project_name": "$PROJECT",
  "state_file": "$state",
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

run_step_from() {
	local dir=$1 cfg=$2 name=$3 want=$4
	shift 4
	local log="$WORK/logs/$name.log" got=0
	(cd "$dir" && "$BIN" --config "$cfg" "$@") >"$log" 2>&1 || got=$?
	if [ "$got" -ne "$want" ]; then
		echo "FAIL $name: exit $got, want $want (log: $log)" >&2
		tail -n 5 "$log" >&2
		exit 1
	fi
	echo "ok   $name (exit $got)"
	sleep 2
}

run_step() {
	local name=$1 want=$2
	shift 2
	run_step_from "$PWD" "$CFG" "$name" "$want" "$@"
}

expect_log() {
	local name=$1 pattern=$2
	if ! grep -q -- "$pattern" "$WORK/logs/$name.log"; then
		echo "FAIL $name: log does not contain '$pattern'" >&2
		exit 1
	fi
}

expect_head() {
	local label=$1 want=$2 got
	got=$(git -C "$REPO" rev-parse HEAD)
	if [ "$got" != "$want" ]; then
		echo "FAIL $label: repo HEAD is $got, want $want" >&2
		exit 1
	fi
}

expect_state_field() {
	local label=$1 file=$2 field=$3 want=$4 got
	got=$(sed -n "s/.*\"$field\": \{0,1\}\(.*\),\{0,1\}$/\1/p" "$file" | tr -d '", ')
	if [ "$got" != "$want" ]; then
		echo "FAIL $label: $field is '$got', want '$want'" >&2
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

write_config "$REL_CFG" "docker-compose.yml" "$REL_STATE"
run_step_from "$WORK" "$REL_CFG" 01b-relative-compose-file 0 sync
expect_log 01b-relative-compose-file '"applied":true'

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

run_step 05-env-file-known-bad 0 sync
expect_log 05-env-file-known-bad 'skipping known-bad commit'

touch "$REPO/app.env"
run_step 06-env-file-fixed 0 --force sync
expect_log 06-env-file-fixed '"applied":true'

run_step 07-git-sync-fails 1 --remote does-not-exist sync
expect_log 07-git-sync-fails 'git sync failed'

write_compose '["sh", "-c", "sleep 3; exit 1"]'
publish "v4: web crashes, no rollback target"
run_step 08-degraded 3 --state-file "$WORK/fresh-state.json" sync
expect_log 08-degraded '"degraded":true'
run_step 09-degraded-refuses 3 --state-file "$WORK/fresh-state.json" sync
expect_log 09-degraded-refuses 'refusing to reconcile'

remove_stack
write_compose '["sleep", "infinity"]'
publish "v5: healthy baseline for crash recovery"
run_step 10-recovery-baseline 0 --state-file "$REC_STATE" sync
expect_log 10-recovery-baseline '"applied":true'
GOOD_COMMIT=$(git -C "$REPO" rev-parse HEAD)

cat >"$SEED/docker-compose.yml" <<'EOF'
services: not-a-valid-services-mapping
EOF
publish "v6: unloadable compose, never synced"
BAD_COMMIT=$(git -C "$SEED" rev-parse HEAD)
git -C "$REPO" fetch -q origin main

cat >"$REC_STATE" <<EOF
{
  "schema_version": 1,
  "last_healthy_commit": "$GOOD_COMMIT",
  "last_result": "success",
  "pending_commit": "$BAD_COMMIT"
}
EOF

run_step 11-recovery-blames-bad-commit 1 --state-file "$REC_STATE" sync
expect_log 11-recovery-blames-bad-commit 'crash recovery'
expect_head 11-recovery-blames-bad-commit "$GOOD_COMMIT"
expect_state_field 11-recovery-blames-bad-commit "$REC_STATE" pending_attempts 1
expect_state_field 11-recovery-blames-bad-commit "$REC_STATE" last_failed_commit "$BAD_COMMIT"

run_step 12-recovery-resumes-revert 1 --state-file "$REC_STATE" sync
expect_log 12-recovery-resumes-revert 'resuming interrupted revert'
expect_head 12-recovery-resumes-revert "$GOOD_COMMIT"
expect_state_field 12-recovery-resumes-revert "$REC_STATE" last_result reverted
expect_state_field 12-recovery-resumes-revert "$REC_STATE" pending_commit ""

run_step 13-recovery-settles-on-known-bad 0 --state-file "$REC_STATE" sync
expect_log 13-recovery-settles-on-known-bad 'skipping known-bad commit'

write_compose '["sleep", "2147483647"]'
publish "v7: healthy change that keeps the health watch busy"
"$BIN" --config "$CFG" --state-file "$LOCK_STATE" sync >"$WORK/logs/15-lock-holder.log" 2>&1 &
HOLDER_PID=$!
sleep 4
run_step 14-lock-skipped 0 --state-file "$LOCK_STATE" sync
expect_log 14-lock-skipped 'holds the state lock'
HOLDER_RC=0
wait "$HOLDER_PID" || HOLDER_RC=$?
if [ "$HOLDER_RC" -ne 0 ]; then
	echo "FAIL 15-lock-holder: exit $HOLDER_RC, want 0 (log: $WORK/logs/15-lock-holder.log)" >&2
	tail -n 5 "$WORK/logs/15-lock-holder.log" >&2
	exit 1
fi
echo "ok   15-lock-holder (exit $HOLDER_RC)"

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
	echo "expect 11 Discord notifications: deployed, deployed, reverted, env_file failure, deployed, git sync failed, DEGRADED, deployed, crash-recovery failure, reverted, deployed"
fi
