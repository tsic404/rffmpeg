#!/usr/bin/env bash
# Shared environment for the acceptance tests under tests/acceptance/.
#
# Every TC script sources this file. It provides: binary build (if missing),
# an isolated Server + Worker on a task-private port, readiness waits, and a
# cleanup that kills only the PIDs it recorded (never bare process names — a
# shared host may run other tasks' servers/workers).
#
# Overridable via the environment:
#   PORT                server port                (default 18999)
#   AUTH_TOKEN          server/worker/client token (default test-token-acceptance)
#   RFFMPEG_BIN         directory holding the three binaries (default <repo>/bin;
#                       when set, the binaries are reused as-is — when unset, the
#                       repo is always rebuilt so stale binaries never serve the run)
#   SERVER_EXTRA_FLAGS  extra rffmpeg-server flags, space-separated
#   HTTP_PORT           port for start_http_fileserver (default PORT+1000)

set -euo pipefail

ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$ENV_DIR/../.." && pwd)"

PORT="${PORT:-18999}"
AUTH_TOKEN="${AUTH_TOKEN:-test-token-acceptance}"
RFFMPEG_BIN_EXPLICIT="${RFFMPEG_BIN:-}"
RFFMPEG_BIN="${RFFMPEG_BIN:-$REPO_ROOT/bin}"

# The run directory is always created by this script (mktemp -d), so cleanup
# can remove it without ever touching a caller-supplied path; the cleanup
# additionally re-checks the path pattern before deleting.
TMP_PARENT="${TMPDIR:-/tmp}"
TMP_PARENT="${TMP_PARENT%/}"
TMP_ROOT="$(mktemp -d "$TMP_PARENT/rffmpeg-acceptance.XXXXXX")"

DATA_DIR="$TMP_ROOT/data"
WORK_DIR="$TMP_ROOT/worker"
MEDIA_DIR="$TMP_ROOT/media"
PIDFILE="$TMP_ROOT/acceptance.pids"
SERVER_LOG="$TMP_ROOT/server.log"
WORKER_LOG="$TMP_ROOT/worker.log"

mkdir -p "$DATA_DIR" "$WORK_DIR" "$MEDIA_DIR"
: > "$PIDFILE"

acceptance_server_url() { printf 'http://127.0.0.1:%s' "$PORT"; }

# build_binaries produces the three contract-named binaries. An explicit
# RFFMPEG_BIN is authoritative and reused as-is (it may point at a build
# produced elsewhere); without one, the repo is always rebuilt so a stale
# bin/ left on the worktree can never pass the run.
build_binaries() {
	if [[ -n "${RFFMPEG_BIN_EXPLICIT:-}" ]]; then
		local f
		for f in rffmpeg rffmpeg-server rffmpeg-worker; do
			[[ -x "$RFFMPEG_BIN/$f" ]] || { echo "RFFMPEG_BIN=$RFFMPEG_BIN lacks $f" >&2; return 1; }
		done
		return 0
	fi
	make -C "$REPO_ROOT" build >/dev/null
	local f
	for f in rffmpeg rffmpeg-server rffmpeg-worker; do
		[[ -x "$RFFMPEG_BIN/$f" ]] || { echo "build did not produce $RFFMPEG_BIN/$f" >&2; return 1; }
	done
}

start_server() {
	local -a extra=()
	if [[ -n "${SERVER_EXTRA_FLAGS:-}" ]]; then
		read -r -a extra <<<"$SERVER_EXTRA_FLAGS"
	fi
	local -a cmd=("$RFFMPEG_BIN/rffmpeg-server"
		--port "$PORT"
		--data-dir "$DATA_DIR"
		--auth-token "$AUTH_TOKEN")
	if ((${#extra[@]} > 0)); then
		cmd+=("${extra[@]}")
	fi
	"${cmd[@]}" >>"$SERVER_LOG" 2>&1 &
	SERVER_PID=$!
	echo "$SERVER_PID" >> "$PIDFILE"
}

# stop_server terminates the server recorded by this run and waits for it, so
# a following start_server can rebind the same port (restart-recovery tests).
stop_server() {
	if [[ -n "${SERVER_PID:-}" ]]; then
		kill "$SERVER_PID" 2>/dev/null || true
		wait "$SERVER_PID" 2>/dev/null || true
	fi
}

start_worker() {
	local name="${1:-acceptance-worker-$PORT}"
	shift || true
	local workdir="$WORK_DIR" log="$WORKER_LOG"
	if [[ "$name" != "acceptance-worker-$PORT" ]]; then
		workdir="$TMP_ROOT/worker-$name"
		log="$TMP_ROOT/worker-$name.log"
		mkdir -p "$workdir"
	fi
	env RFFMPEG_SERVER_URL="$(acceptance_server_url)" \
		RFFMPEG_TOKEN="$AUTH_TOKEN" \
		RFFMPEG_WORKER_NAME="$name" \
		RFFMPEG_TEMP_DIR="$workdir" \
		RFFMPEG_CACHE_ENABLED=false \
		"$@" \
		"$RFFMPEG_BIN/rffmpeg-worker" >>"$log" 2>&1 &
	WORKER_PID=$!
	echo "$WORKER_PID" >> "$PIDFILE"
	printf '%s %s\n' "$name" "$WORKER_PID" >> "$TMP_ROOT/workers.pids"
}

# worker_pid prints the PID recorded for a named worker (empty when absent).
worker_pid() {
	awk -v name="$1" '$1 == name { pid = $2 } END { if (pid != "") print pid }' \
		"$TMP_ROOT/workers.pids" 2>/dev/null || true
}

# wait_server_ready blocks until the health endpoint answers. Tests that
# intentionally run without a Worker use this instead of wait_ready.
wait_server_ready() {
	local deadline=$((SECONDS + 30))
	until curl -fsS -o /dev/null "$(acceptance_server_url)/health"; do
		((SECONDS < deadline)) || { echo "server did not become healthy; see $SERVER_LOG" >&2; return 1; }
		sleep 0.2
	done
}

# wait_ready blocks until the health endpoint answers AND the worker is
# registered and not offline.
wait_ready() {
	wait_server_ready
	local deadline=$((SECONDS + 30))
	local workers
	until workers="$(curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$(acceptance_server_url)/api/v1/workers")" &&
		grep -q '"status":"online"\|"status":"idle"\|"status":"busy"' <<<"$workers"; do
		((SECONDS < deadline)) || { echo "worker did not register; see $WORKER_LOG" >&2; return 1; }
		sleep 0.2
	done
}

# wait_worker blocks until the named worker appears in the workers list.
wait_worker() {
	local name="$1" deadline=$((SECONDS + 30))
	until api_get /api/v1/workers | grep -q "\"name\":\"$name\""; do
		((SECONDS < deadline)) || { echo "worker $name did not register" >&2; return 1; }
		sleep 0.2
	done
}

# api_get performs an authenticated GET against the run's server.
api_get() {
	curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$(acceptance_server_url)$1"
}

# now_ns prints a monotonic nanosecond timestamp. busybox date has no %N, so
# python3's monotonic clock is the primary source; a nanosecond-capable date is
# only a fallback when python3 is missing.
if command -v python3 >/dev/null; then
	now_ns() { python3 -c 'import time; print(int(time.monotonic() * 1e9))'; }
elif command -v date >/dev/null && [[ "$(date +%s%N)" =~ ^[0-9]{19}$ ]]; then
	now_ns() { date +%s%N; }
else
	now_ns() { echo "FATAL: neither python3 nor a nanosecond-capable date is available" >&2; exit 1; }
fi

# job_field prints one field of a job object ("job" envelope unwrapped).
job_field() {
	api_get "/api/v1/jobs/$1" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["job"].get(sys.argv[1]))' "$2"
}

# job_id_from_log extracts the submitted job id from a CLI log. The trailing
# `|| true` keeps an empty/missing log from failing the caller under pipefail.
job_id_from_log() {
	grep -oE 'Job submitted: [0-9a-f-]+' "$1" 2>/dev/null | awk '{print $3}' || true
}

# wait_job_status blocks until the job reaches one of the space-separated
# statuses and prints it; the timeout is the second argument, in seconds.
wait_job_status() {
	local id="$1" budget="${2:-300}" want="$3" status=""
	local deadline=$((SECONDS + budget))
	while :; do
		status="$(job_field "$id" status 2>/dev/null || true)"
		case " $want " in *" $status "*) printf '%s' "$status"; return 0 ;; esac
		((SECONDS < deadline)) || { echo "job $id stayed $status; want one of: $want" >&2; return 1; }
		sleep 0.5
	done
}

# make_media renders a silent H.264 test clip and prints its path. Repeated
# calls in one run reuse the file (same name → same content).
make_media() {
	local path="$MEDIA_DIR/$1" duration="$2" size="${3:-640x480}"
	if [[ ! -f "$path" ]]; then
		ffmpeg -loglevel error -y -f lavfi \
			-i "testsrc=duration=$duration:size=$size:rate=24" \
			-c:v libx264 -preset ultrafast -pix_fmt yuv420p "$path"
	fi
	printf '%s' "$path"
}

# make_media_av renders a test clip with a sine audio track.
make_media_av() {
	local path="$MEDIA_DIR/$1" duration="$2" size="${3:-640x480}"
	if [[ ! -f "$path" ]]; then
		ffmpeg -loglevel error -y \
			-f lavfi -i "testsrc=duration=$duration:size=$size:rate=24" \
			-f lavfi -i "sine=frequency=440:duration=$duration" \
			-c:v libx264 -preset ultrafast -pix_fmt yuv420p \
			-c:a aac -shortest "$path"
	fi
	printf '%s' "$path"
}

# make_audio renders an audio-only AAC test clip.
make_audio() {
	local path="$MEDIA_DIR/$1" duration="$2"
	if [[ ! -f "$path" ]]; then
		ffmpeg -loglevel error -y -f lavfi -i "sine=frequency=440:duration=$duration" \
			-c:a aac "$path"
	fi
	printf '%s' "$path"
}

# start_http_fileserver serves MEDIA_DIR (or the given directory) over HTTP so
# remote-URL input tests exercise the Worker's own download path. Sets
# HTTP_PORT and records the PID for cleanup. The default port leaves a wide
# gap from PORT so concurrently running TC scripts (which take consecutive
# server ports) cannot collide with a neighbour's file server.
start_http_fileserver() {
	local dir="${1:-$MEDIA_DIR}"
	HTTP_PORT="${HTTP_PORT:-$((PORT + 1000))}"
	python3 -m http.server "$HTTP_PORT" --bind 127.0.0.1 --directory "$dir" \
		>>"$TMP_ROOT/http.log" 2>&1 &
	HTTP_PID=$!
	echo "$HTTP_PID" >> "$PIDFILE"
	local deadline=$((SECONDS + 15))
	until curl -fsS -o /dev/null "http://127.0.0.1:$HTTP_PORT/" 2>/dev/null; do
		((SECONDS < deadline)) || { echo "http fileserver did not start; see $TMP_ROOT/http.log" >&2; return 1; }
		sleep 0.2
	done
}

# fail prints the failure reason and exits non-zero.
fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# expect_rc asserts an exact exit code.
expect_rc() {
	[[ "$1" == "$2" ]] || fail "$3: rc=$1, want $2"
}

# expect_in_file asserts a fixed substring appears in a file.
expect_in_file() {
	grep -qF -- "$2" "$1" || fail "$1 lacks expected text: $2"
}

# expect_no_panic guards the user-facing error paths: no panic, no goroutine
# dump, no stack trace.
expect_no_panic() {
	! grep -qE 'panic:|goroutine [0-9]+ \[|runtime error' "$1" ||
		fail "$1 contains a panic/stack trace"
}

# acceptance_cleanup kills only the PIDs recorded by this run and removes the
# run directory this script created. Safe to call twice.
acceptance_cleanup() {
	local pid
	if [[ -s "$PIDFILE" ]]; then
		while IFS= read -r pid; do
			[[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
		done < "$PIDFILE"
		: > "$PIDFILE"
	fi
	case "$TMP_ROOT" in
	"$TMP_PARENT"/rffmpeg-acceptance.*) rm -rf -- "$TMP_ROOT" ;;
	*) echo "refusing to remove unexpected run directory: $TMP_ROOT" >&2 ;;
	esac
}

trap acceptance_cleanup EXIT
