#!/usr/bin/env bash
# TC-error-05: Server 宕机（AC-15）
# 判据：3s 内返回连接错误，不挂死。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
MEDIA="$(make_media down.mp4 2)"
LOG="$TMP_ROOT/cli.log"

# No server is started: the port stays closed for the whole probe.
START=$(now_ns)
set +e
"$RFFMPEG_BIN/rffmpeg" --server "http://127.0.0.1:$PORT" --token "$AUTH_TOKEN" \
	-i "$MEDIA" "$TMP_ROOT/out.mp4" >/dev/null 2>"$LOG"
RC=$?
set -e
WALL_MS=$(( ($(now_ns) - START) / 1000000 ))

expect_rc "$RC" 1 "server-down rc"
expect_in_file "$LOG" "server health check failed"
(( WALL_MS <= 3000 )) || fail "connection error took ${WALL_MS}ms, want <= 3000ms"

echo "PASS: rc=1, connection error returned in ${WALL_MS}ms"
