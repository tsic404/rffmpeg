#!/usr/bin/env bash
# TC-security-02: 错误 token（AC-27）
# 判据：HTTP 401 且提示明确。
#
# 客户端健康探测携带已配置的令牌（C-9），因此错误令牌在探测阶段就被拒绝，
# 走健康探测失败行（C-25），不会走到首个业务请求。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media badauth.mp4 2)"
OUT="$TMP_ROOT/out.mp4"
LOG="$TMP_ROOT/cli.log"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "definitely-wrong" \
	-i "$MEDIA" "$OUT" >/dev/null 2>"$LOG"
RC=$?
set -e

expect_rc "$RC" 1 "wrong-token rc"
expect_in_file "$LOG" "server health check failed: server unhealthy: status 401"
expect_no_panic "$LOG"
[[ ! -e "$OUT" ]] || fail "output produced with a wrong token"

echo "PASS: rc=1, health probe rejected with HTTP 401"
