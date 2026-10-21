#!/usr/bin/env bash
# TC-error-02: 参数不完整（编码器选项缺值）（AC-12）
# 判据：ffmpeg 风格的错误提示；无 panic；不卡死。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media args.mp4 2)"
LOG="$TMP_ROOT/cli.log"

START=$(now_ns)
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v >/dev/null 2>"$LOG"
RC=$?
set -e
WALL_MS=$(( ($(now_ns) - START) / 1000000 ))

expect_rc "$RC" 1 "incomplete-args rc"
expect_in_file "$LOG" "requires a value"
expect_in_file "$LOG" "-c:v"
expect_no_panic "$LOG"
(( WALL_MS < 30000 )) || fail "CLI hung for ${WALL_MS}ms"

echo "PASS: rc=1, ffmpeg-style error, no panic, wall=${WALL_MS}ms"
