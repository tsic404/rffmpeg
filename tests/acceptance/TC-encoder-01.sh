#!/usr/bin/env bash
# TC-encoder-01: 未指定编码器 → 自动硬件加速（AC-21、F3 负向）
# 判据：stderr 至少含 `[rffmpeg] auto-selected <name>` 与
# `[rffmpeg] INFO: Worker capabilities: ...`；不断言具体编码器名。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media auto.mp4 4)"
OUT="$TMP_ROOT/auto.mp4"
LOG="$TMP_ROOT/cli.log"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" "$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "auto-selected transcode rc"

grep -qE '^\[rffmpeg\] auto-selected [A-Za-z0-9_.-]+$' "$LOG" ||
	fail "missing '[rffmpeg] auto-selected <name>' line"
grep -qF '[rffmpeg] INFO: Worker capabilities:' "$LOG" ||
	fail "missing '[rffmpeg] INFO: Worker capabilities:' line"
[[ -f "$OUT" ]] || fail "output missing"

echo "PASS: rc=0, auto-selected line and capabilities line present"
