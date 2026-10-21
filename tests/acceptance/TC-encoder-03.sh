#!/usr/bin/env bash
# TC-encoder-03: 自动升级开关开 / 关对比（AC-23、F4 负向）
# 判据：开启时输出 `[rffmpeg] upgraded libx264 → <name>`；关闭时输出
# `[rffmpeg] libx264 -> libx264 (Pass through unchanged)` 且编码器不变。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media autohw.mp4 4)"

OFF_LOG="$TMP_ROOT/off.log"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 "$TMP_ROOT/off.mp4" 2>"$OFF_LOG"
RC=$?
set -e
expect_rc "$RC" 0 "pass-through rc"
grep -qxF '[rffmpeg] libx264 -> libx264 (Pass through unchanged)' "$OFF_LOG" ||
	fail "missing pass-through line without --auto-hw (F4)"
CODE="$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of default=nw=1:nk=1 "$TMP_ROOT/off.mp4")"
[[ "$CODE" == "h264" ]] || fail "encoder changed without --auto-hw: $CODE"

ENCODERS="$(api_get /api/v1/workers |
	python3 -c 'import json,sys; print(" ".join(sorted({e for w in json.load(sys.stdin)["workers"] for e in w["encoders"]})))')"
HW=""
for candidate in h264_nvenc h264_qsv h264_amf h264_vaapi h264_vulkan; do
	if grep -qw "$candidate" <<<"$ENCODERS"; then HW="$candidate"; break; fi
done

ON_LOG="$TMP_ROOT/on.log"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	--auto-hw -i "$MEDIA" -c:v libx264 "$TMP_ROOT/on.mp4" 2>"$ON_LOG"
RC=$?
set -e
expect_rc "$RC" 0 "auto-hw rc"

if [[ -n "$HW" ]]; then
	grep -qE '^\[rffmpeg\] upgraded libx264 → [A-Za-z0-9_.-]+$' "$ON_LOG" ||
		fail "missing upgrade line with --auto-hw"
else
	echo "NOTE: no hardware H.264 encoder reported by the worker; upgrade line not observable"
fi

echo "PASS: pass-through without --auto-hw; upgrade path exercised"
