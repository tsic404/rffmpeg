#!/usr/bin/env bash
# TC-encoder-05: Worker 能力上报与调度匹配（AC-25）
# 判据：encoders 为全量视频编码器且不带 [HW] 标记；hwaccels 为 ffmpeg 全量
# 文本；[HW] 只出现在 CLI stderr 的 capabilities 行。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

api_get /api/v1/workers |
	python3 -c '
import json, sys
w = json.load(sys.stdin)["workers"][0]
assert w["encoders"], "empty encoder list"
assert all("[HW]" not in e for e in w["encoders"]), "server encoder list carries [HW] markers"
assert "libx264" in w["encoders"], "libx264 missing from the video encoder list"
assert "aac" not in w["encoders"], "audio encoder leaked into the video encoder list"
assert w["ffmpeg_version"], "ffmpeg_version missing"
assert "Hardware acceleration methods:" in w["hwaccels"], "hwaccels is not the full ffmpeg text"
assert w["last_heartbeat"], "last_heartbeat missing at the worker top level"
' || fail "worker capability contract violated"

MEDIA="$(make_media caps.mp4 2)"
LOG="$TMP_ROOT/cli.log"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" "$TMP_ROOT/out.mp4" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "capabilities transcode rc"

CAPS="$(grep -F '[rffmpeg] INFO: Worker capabilities:' "$LOG" || true)"
[[ -n "$CAPS" ]] || fail "CLI capabilities line missing"

# [HW] marks live only on the CLI line, and only when the worker exposes an
# accessible GPU device.
HAS_GPU="$(api_get /api/v1/workers |
	python3 -c 'import json,sys; w=json.load(sys.stdin)["workers"][0]; print("yes" if any(d.get("accessible") for d in w.get("gpu_devices", [])) else "no")')"
if [[ "$HAS_GPU" == "yes" ]]; then
	grep -qF '[HW]' <<<"$CAPS" || fail "CLI capabilities line lacks [HW] hardware markers"
else
	echo "NOTE: no accessible GPU device; [HW] marker not expected"
fi

echo "PASS: server list full and marker-free, hwaccels full text, [HW] only on CLI"
