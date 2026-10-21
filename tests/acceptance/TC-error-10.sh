#!/usr/bin/env bash
# TC-error-10: Server / Worker 重启恢复（AC-20）
# 判据：已提交任务不丢失（rc=0 + ffprobe 正常）；恢复后的 job 走完整重编
# （stderr 有 ffmpeg 进度输出、无 Cache hit 行）。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media restart.mp4 120 1280x720)"
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

stop_server
kill -9 "$(worker_pid "acceptance-worker-$PORT")" || fail "worker pid not recorded"
sleep 1

start_server
wait_server_ready
# The recovery worker runs with its own cache dir so the "no Cache hit"
# assertion is observable: a recovered job wrongly served from cache would
# print a hit line.
start_worker recovery-worker RFFMPEG_CACHE_ENABLED=true \
	RFFMPEG_CACHE_DIR="$TMP_ROOT/recovery-cache"
wait_worker recovery-worker

DEADLINE=$((SECONDS + 180))
while (( SECONDS < DEADLINE )); do
	STATUS="$(job_field "$JOB" status 2>/dev/null || true)"
	case "$STATUS" in
	completed | failed | cancelled | timeout) break ;;
	esac
	sleep 1
done
set +e
wait "$CLI_PID"
RC=$?
set -e
expect_rc "$RC" 0 "restart-recovery rc"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 120.0) <= 0.2 else 1)' "$DUR" ||
	fail "recovered output duration $DUR, want 120.0"
grep -qE '^Progress: [0-9]+\.[0-9]% \| ETA:' "$LOG" || fail "no ffmpeg progress output after recovery"
grep -q '\[rffmpeg\] Cache hit' "$LOG" && fail "recovered job served from cache; full re-encode required"

echo "PASS: rc=0 after server/worker restart, duration=$DUR, full re-encode"
