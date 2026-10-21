#!/usr/bin/env bash
# TC-security-01: 未提供 token（AC-26、F1 负向）
# 判据：rc≠0、HTTP 401、绝不回退本地 ffmpeg 执行。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media noauth.mp4 2)"
OUT="$TMP_ROOT/out.mp4"
LOG="$TMP_ROOT/cli.log"

set +e
env -u RFFMPEG_TOKEN "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" \
	-i "$MEDIA" "$OUT" >/dev/null 2>"$LOG"
RC=$?
set -e

[[ $RC -ne 0 ]] || fail "missing token was accepted (rc=0)"
expect_in_file "$LOG" "401"
[[ ! -e "$OUT" ]] || fail "output produced without authentication"
! grep -qE 'Output #0|Stream mapping:|frame=.*fps=' "$LOG" ||
	fail "local ffmpeg ran after auth failure (F1)"

echo "PASS: rc=$RC, HTTP 401, no local fallback"
