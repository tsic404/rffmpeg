#!/usr/bin/env bash
# TC-datachan-03: 可流式容器输出到 stdout（AC-34）
# 判据：rc=0、输出为有效 MP4 且非零字节、数据实时流回、Ctrl+C 后 Worker 停止。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media stream4.mp4 4)"
OUT="$TMP_ROOT/stream.mp4"
LOG="$TMP_ROOT/stream.log"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -f mp4 - >"$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "streaming rc"

[[ -s "$OUT" ]] || fail "stream produced no bytes"
FMT="$(ffprobe -v error -show_entries format=format_name -of default=nw=1:nk=1 "$OUT")"
grep -q 'mp4' <<<"$FMT" || fail "stdout is not a valid MP4: $FMT"
DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 4.0) <= 0.1 else 1)' "$DUR" ||
	fail "unexpected duration: $DUR"

# Real-time flow and cancellation: bytes must arrive while the job still runs.
LONG="$(make_media stream-long.mp4 120 1280x720)"
INT_OUT="$TMP_ROOT/interrupted.mp4"
INT_LOG="$TMP_ROOT/interrupted.log"
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$LONG" -c:v libx264 -preset veryslow -f mp4 - >"$INT_OUT" 2>"$INT_LOG" &
CLI_PID=$!
trap 'kill "$CLI_PID" 2>/dev/null || true; acceptance_cleanup' EXIT

DEADLINE=$((SECONDS + 40))
until [[ -s "$INT_OUT" ]]; do
	kill -0 "$CLI_PID" 2>/dev/null || fail "streaming CLI exited before producing any bytes"
	(( SECONDS < DEADLINE )) || fail "no streamed bytes within 40s (not real-time)"
	sleep 0.2
done

kill -INT "$CLI_PID"
set +e
wait "$CLI_PID"
INT_RC=$?
set -e
[[ $INT_RC -ne 0 ]] || fail "interrupted streaming run reported success"

WPID="$(worker_pid "acceptance-worker-$PORT")"
DEADLINE=$((SECONDS + 15))
while [[ -n "$(pgrep -P "$WPID" -f ffmpeg 2>/dev/null || true)" ]]; do
	(( SECONDS < DEADLINE )) || fail "Worker kept an ffmpeg process after Ctrl+C"
	sleep 0.5
done
grep -qE 'Job was cancelled|Job status: cancelled' "$INT_LOG" || fail "cancel not reported after Ctrl+C"

echo "PASS: valid MP4 stream, real-time bytes, Worker stopped after Ctrl+C"
