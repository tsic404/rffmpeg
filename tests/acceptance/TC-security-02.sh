#!/usr/bin/env bash
# TC-security-02: 错误 token（AC-27）
# 判据：HTTP 401 且提示明确。
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
expect_in_file "$LOG" "authentication rejected (HTTP 401)"
expect_no_panic "$LOG"
[[ ! -e "$OUT" ]] || fail "output produced with a wrong token"

echo "PASS: rc=1, explicit HTTP 401 rejection"
