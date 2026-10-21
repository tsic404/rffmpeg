#!/usr/bin/env bash
# TC-error-04: 空命令调用（AC-14）
# 判据：显示帮助；无 panic。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries

LOG="$TMP_ROOT/cli.log"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "http://127.0.0.1:$PORT" --token "$AUTH_TOKEN" \
	>/dev/null 2>"$LOG"
RC=$?
set -e

[[ $RC -ne 0 ]] || fail "empty command exited 0; help is a non-zero exit"
expect_in_file "$LOG" "Usage:"
expect_no_panic "$LOG"

echo "PASS: help displayed, rc=$RC, no panic"
