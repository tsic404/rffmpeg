#!/usr/bin/env bash
# TC-error-01: 输入文件不存在（AC-11）
# 判据：提示含文件名；无 panic / 堆栈；不卡死。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MISSING="$MEDIA_DIR/definitely-missing-input.mp4"
OUT="$TMP_ROOT/out.mp4"
LOG="$TMP_ROOT/cli.log"

START=$(now_ns)
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MISSING" "$OUT" >/dev/null 2>"$LOG"
RC=$?
set -e
WALL_MS=$(( ($(now_ns) - START) / 1000000 ))

expect_rc "$RC" 1 "missing-input rc"
expect_in_file "$LOG" "definitely-missing-input.mp4"
expect_no_panic "$LOG"
[[ ! -e "$OUT" ]] || fail "output created despite missing input"
(( WALL_MS < 30000 )) || fail "CLI hung for ${WALL_MS}ms"

echo "PASS: rc=1, filename reported, no panic, wall=${WALL_MS}ms"
