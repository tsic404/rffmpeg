#!/usr/bin/env bash
# Shared environment for the acceptance tests under tests/acceptance/.
#
# Every TC script sources this file. It provides: binary build (if missing),
# an isolated Server + Worker on a task-private port, readiness waits, and a
# cleanup that kills only the PIDs it recorded (never bare process names — a
# shared host may run other tasks' servers/workers).
#
# Overridable via the environment:
#   PORT          server port                (default 18999)
#   AUTH_TOKEN    server/worker/client token (default test-token-acceptance)
#   RFFMPEG_BIN   directory holding the three binaries (default <repo>/bin;
#                 when set, the binaries are reused as-is — when unset, the
#                 repo is always rebuilt so stale binaries never serve the run)

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
	"$RFFMPEG_BIN/rffmpeg-server" \
		--port "$PORT" \
		--data-dir "$DATA_DIR" \
		--auth-token "$AUTH_TOKEN" \
		>"$SERVER_LOG" 2>&1 &
	SERVER_PID=$!
	echo "$SERVER_PID" >> "$PIDFILE"
}

start_worker() {
	RFFMPEG_SERVER_URL="$(acceptance_server_url)" \
	RFFMPEG_TOKEN="$AUTH_TOKEN" \
	RFFMPEG_WORKER_NAME="acceptance-worker-$PORT" \
	RFFMPEG_TEMP_DIR="$WORK_DIR" \
	RFFMPEG_CACHE_ENABLED=false \
		"$RFFMPEG_BIN/rffmpeg-worker" \
		>"$WORKER_LOG" 2>&1 &
	WORKER_PID=$!
	echo "$WORKER_PID" >> "$PIDFILE"
}

# wait_ready blocks until the health endpoint answers AND the worker is
# registered and not offline.
wait_ready() {
	local deadline=$((SECONDS + 30))
	until curl -fsS -o /dev/null "$(acceptance_server_url)/health"; do
		((SECONDS < deadline)) || { echo "server did not become healthy; see $SERVER_LOG" >&2; return 1; }
		sleep 0.2
	done
	local workers
	until workers="$(curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$(acceptance_server_url)/api/v1/workers")" &&
		grep -q '"status":"online"\|"status":"idle"\|"status":"busy"' <<<"$workers"; do
		((SECONDS < deadline)) || { echo "worker did not register; see $WORKER_LOG" >&2; return 1; }
		sleep 0.2
	done
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
