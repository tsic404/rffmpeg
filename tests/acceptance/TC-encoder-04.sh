#!/usr/bin/env bash
# TC-encoder-04: crf / preset 语义映射与参数去向行（AC-24）
# 判据：`[rffmpeg] <target> params:` 行存在；-crf 与 -preset 各有映射记录，
# 不支持者被标注；不做 SSIM/PSNR 计算。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media params.mp4 4)"
ENCODERS="$(api_get /api/v1/workers |
	python3 -c 'import json,sys; print(" ".join(sorted({e for w in json.load(sys.stdin)["workers"] for e in w["encoders"]})))')"

TARGET=""
for candidate in h264_nvenc h264_qsv h264_amf h264_vulkan h264_vaapi; do
	if ! grep -qw "$candidate" <<<"$ENCODERS"; then TARGET="$candidate"; break; fi
done
[[ -n "$TARGET" ]] || fail "every H.264 hardware encoder is available; no rewrite target"

LOG="$TMP_ROOT/cli.log"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v "$TARGET" -crf 23 -preset fast -tune film \
	"$TMP_ROOT/params-out.mp4" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "params-mapping rc"

PARAMS="$(grep -E '^\[rffmpeg\] [A-Za-z0-9_.-]+ params: ' "$LOG" || true)"
[[ -n "$PARAMS" ]] || fail "missing '[rffmpeg] <target> params:' line"
grep -qE -- '-crf 23 (→|->) ' <<<"$PARAMS" || fail "-crf has no target mapping in: $PARAMS"
grep -qE -- '-preset fast (→|\(not supported by )' <<<"$PARAMS" ||
	fail "-preset is neither mapped nor annotated in: $PARAMS"
grep -qE 'SSIM|PSNR' "$LOG" && fail "quality computation (SSIM/PSNR) must not run"

echo "PASS: params line present with crf/preset disposition"
