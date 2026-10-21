#!/usr/bin/env bash
# TC-observability-05: 9 种失败分类与 retryable（AC-42）
# 判据：可触发分类逐项正确、retryable 与契约全表一致；WORKER_CRASH 两条分支
# 均覆盖。DISK_FULL / INFRA 的 E2E 不可达项以实现侧单测覆盖并在报告注明。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

fail_job() {
	local id="$1" want_type="$2" want_retry="$3"
api_get "/api/v1/jobs/$id" |
	python3 -c '
import json, sys
job = json.load(sys.stdin)["job"]
want_type, want_retry = sys.argv[1], sys.argv[2] == "true"
assert job["status"] in ("failed", "timeout"), (job["status"], job.get("failure_type"))
assert job["failure_type"] == want_type, (job["failure_type"], want_type)
assert job["retryable"] is want_retry, (job["retryable"], want_retry)
' "$want_type" "$want_retry" || fail "job $id is not $want_type/retryable=$want_retry"
}

job_id_of() {
	grep -oE 'Job submitted: [0-9a-f-]+' "$1" 2>/dev/null | awk '{print $3}' || true
}

SHORT="$(make_media short.mp4 2)"
LONG="$(make_media long.mp4 120 1280x720)"

# --- INPUT_UNREACHABLE
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "http://127.0.0.1:9/unreachable.mp4" "$TMP_ROOT/u.mp4" >/dev/null 2>"$TMP_ROOT/u.log" || true
fail_job "$(job_id_of "$TMP_ROOT/u.log")" INPUT_UNREACHABLE false

# --- ENCODER_UNAVAILABLE (submission guard)
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$SHORT" -c:v totes_not_a_codec "$TMP_ROOT/e1.mp4" >/dev/null 2>"$TMP_ROOT/e1.log" || true
fail_job "$(job_id_of "$TMP_ROOT/e1.log")" ENCODER_UNAVAILABLE false

# --- ENCODER_UNSUPPORTED (execution-side rewrite)
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$SHORT" -c:v libsvtav1_nonexist "$TMP_ROOT/e2.mp4" >/dev/null 2>"$TMP_ROOT/e2.log" || true
fail_job "$(job_id_of "$TMP_ROOT/e2.log")" ENCODER_UNSUPPORTED false

# --- FFMPEG_ERROR (invalid filter makes ffmpeg fail)
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$SHORT" -vf "no_such_filter_xyz=1" "$TMP_ROOT/e3.mp4" >/dev/null 2>"$TMP_ROOT/e3.log" || true
fail_job "$(job_id_of "$TMP_ROOT/e3.log")" FFMPEG_ERROR false

# --- TIMEOUT (execution budget shorter than the encode)
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	--timeout 3s -i "$LONG" -c:v libx264 -preset veryslow "$TMP_ROOT/e4.mp4" \
	>/dev/null 2>"$TMP_ROOT/e4.log" || true
fail_job "$(job_id_of "$TMP_ROOT/e4.log")" TIMEOUT true

# --- WORKER_CRASH, Worker-side branch: kill the ffmpeg child until the
# retry budget is exhausted; the Worker itself stays alive.
WPID="$(worker_pid "acceptance-worker-$PORT")"
( timeout 180 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$LONG" -c:v libx264 -preset veryslow "$TMP_ROOT/e5.mp4" >/dev/null 2>"$TMP_ROOT/e5.log" ) &
CLI_PID=$!
trap 'kill "$CLI_PID" 2>/dev/null || true; acceptance_cleanup' EXIT
E5_JOB=""
for _ in $(seq 1 120); do
	E5_JOB="$(job_id_of "$TMP_ROOT/e5.log" 2>/dev/null || true)"
	[[ -n "$E5_JOB" ]] && break
	sleep 0.5
done
[[ -n "$E5_JOB" ]] || fail "worker-crash job not submitted"
wait_job_status "$E5_JOB" 60 "running" >/dev/null || fail "worker-crash job never ran"
for _ in $(seq 1 60); do
	CHILD="$(pgrep -P "$WPID" -f ffmpeg | head -1 || true)"
	[[ -n "$CHILD" ]] && kill -9 "$CHILD" 2>/dev/null || true
	kill -0 "$CLI_PID" 2>/dev/null || break
	sleep 1.2
done
wait "$CLI_PID" 2>/dev/null || true
fail_job "$(job_id_of "$TMP_ROOT/e5.log")" WORKER_CRASH true

# --- WORKER_CRASH (Server-side sweep) and NO_WORKER_AVAILABLE
kill -9 "$WPID" 2>/dev/null || true
stop_server
SERVER_EXTRA_FLAGS="--worker-heartbeat-timeout 8s --worker-health-check-interval 2s --no-worker-job-timeout 20s --timeout-check-interval 5s"
start_server
wait_server_ready
start_worker sweep-worker RFFMPEG_HEARTBEAT_INTERVAL=2s
wait_worker sweep-worker
SWEEP_PID="$(worker_pid sweep-worker)"

( timeout 150 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$LONG" -c:v libx264 -preset veryslow "$TMP_ROOT/s1.mp4" >/dev/null 2>"$TMP_ROOT/s1.log" ) &
A_PID=$!
trap 'kill "$A_PID" 2>/dev/null || true; acceptance_cleanup' EXIT

A_JOB=""
for _ in $(seq 1 120); do
	A_JOB="$(job_id_of "$TMP_ROOT/s1.log" 2>/dev/null || true)"
	[[ -n "$A_JOB" ]] && break
	sleep 0.5
done
[[ -n "$A_JOB" ]] || fail "crash scenario: long job not submitted"
wait_job_status "$A_JOB" 30 "running" >/dev/null || fail "crash scenario: long job never ran"

# Only after the worker is busy does the second job stay pending, unclaimed.
( timeout 150 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$SHORT" -c:v libx264 "$TMP_ROOT/s2.mp4" >/dev/null 2>"$TMP_ROOT/s2.log" ) &
B_PID=$!
trap 'kill "$A_PID" "$B_PID" 2>/dev/null || true; acceptance_cleanup' EXIT

B_JOB=""
for _ in $(seq 1 120); do
	B_JOB="$(job_id_of "$TMP_ROOT/s2.log" 2>/dev/null || true)"
	[[ -n "$B_JOB" ]] && break
	sleep 0.5
done
[[ -n "$B_JOB" ]] || fail "crash scenario: pending job not submitted"
wait_job_status "$B_JOB" 30 "pending" >/dev/null || fail "crash scenario: second job was claimed"

kill -9 "$SWEEP_PID" 2>/dev/null || true
set +e
wait "$A_PID" "$B_PID" 2>/dev/null
set -e

fail_job "$A_JOB" WORKER_CRASH true
fail_job "$B_JOB" NO_WORKER_AVAILABLE false

echo "NOTE: DISK_FULL and INFRA are not E2E-reachable; covered by implementation-side unit tests"
echo "PASS: 7 reachable failure classes correct, both WORKER_CRASH branches covered"
