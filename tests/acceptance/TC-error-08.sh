#!/usr/bin/env bash
# TC-error-08: Worker 执行中崩溃（AC-18、F7 负向）
# 判据：CLI 报任务失败；输出文件不存在或可被 ffprobe 正常读取（无半成品）。
set -euo pipefail
PORT="${PORT:-18999}"
SERVER_EXTRA_FLAGS="--worker-heartbeat-timeout 8s --worker-health-check-interval 2s --no-worker-job-timeout 20s"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker acceptance-worker-$PORT RFFMPEG_HEARTBEAT_INTERVAL=2s
wait_ready

MEDIA="$(make_media crash.mp4 120 1280x720)"
OUT="$TMP_ROOT/out.mp4"
LOG="$TMP_ROOT/cli.log"

timeout 200 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -preset veryslow -threads 2 "$OUT" >/dev/null 2>"$LOG" &
CLI_PID=$!
trap 'kill "$CLI_PID" 2>/dev/null || true; acceptance_cleanup' EXIT

JOB=""
for _ in $(seq 1 120); do
	JOB="$(job_id_from_log "$LOG")"
	[[ -n "$JOB" ]] && break
	sleep 0.5
done
[[ -n "$JOB" ]] || fail "job id not reported"
wait_job_status "$JOB" 30 "running" >/dev/null || fail "job never reached running"

kill -9 "$(worker_pid "acceptance-worker-$PORT")" || fail "worker pid not recorded"

set +e
wait "$CLI_PID"
RC=$?
set -e
expect_rc "$RC" 1 "worker-crash rc"
grep -q 'Job failed:' "$LOG" || fail "CLI did not report a failed job"

# F7: no half-written output. A present file must be a complete, probeable one.
if [[ -e "$OUT" ]]; then
	ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT" >/dev/null ||
		fail "partial/corrupt output left behind"
fi

echo "PASS: rc=1 after worker crash, no partial output"
