#!/usr/bin/env bash
# TC-observability-01: probe 注入 _rffmpeg 且不污染原始字段（AC-38）
# 判据：响应含注入键与建议；原始 streams 字段不变；同步返回。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media probe.mp4 4)"
OUT="$TMP_ROOT/probe.json"
LOG="$TMP_ROOT/probe.log"

START=$(now_ns)
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	probe "$MEDIA" >"$OUT" 2>"$LOG"
RC=$?
set -e
WALL_MS=$(( ($(now_ns) - START) / 1000000 ))
expect_rc "$RC" 0 "probe rc"
(( WALL_MS < 10000 )) || fail "probe took ${WALL_MS}ms; it must return synchronously"

ffprobe -v error -show_entries format=format_name,duration -show_streams -of json "$MEDIA" \
	>"$TMP_ROOT/ffprobe.json"

python3 - "$OUT" "$TMP_ROOT/ffprobe.json" <<'PY' || fail "probe response contract violated"
import json, sys

resp = json.load(open(sys.argv[1]))
local = json.load(open(sys.argv[2]))

assert set(resp) == {"format", "streams", "_rffmpeg"}, sorted(resp)

ext = resp["_rffmpeg"]
assert isinstance(ext.get("worker_encoders"), list) and ext["worker_encoders"], "worker_encoders missing"
assert "suggestion" in ext, "suggestion key missing"

assert resp["format"]["format_name"] == local["format"]["format_name"]
remote_stream = resp["streams"][0]
local_stream = local["streams"][0]
for key in ("codec_name", "codec_type", "width", "height"):
    assert remote_stream.get(key) == local_stream.get(key), (key, remote_stream.get(key), local_stream.get(key))
PY

echo "PASS: _rffmpeg injected, streams unpolluted, synchronous (${WALL_MS}ms)"
