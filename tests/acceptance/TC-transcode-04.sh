#!/usr/bin/env bash
# TC-transcode-04: 编码器与质量选项正确传递（AC-07）
# 判据：不报错；输出可播放；-crf 在 x264 配置行中生效；-b:a 反映在音频码率上。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media_av av.mp4 4)"
OUT="$TMP_ROOT/quality.mp4"
LOG="$TMP_ROOT/cli.log"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -preset veryfast -crf 28 -c:a aac -b:a 96k \
	"$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "quality-options transcode rc"

STREAMS="$(ffprobe -v error -show_entries stream=codec_type,codec_name -of csv=p=0 "$OUT")"
grep -q '^h264,video$' <<<"$STREAMS" || fail "video stream missing: $STREAMS"
grep -q '^aac,audio$' <<<"$STREAMS" || fail "audio stream missing: $STREAMS"

# x264 echoes its effective CRF in the encoder configuration line.
grep -q 'crf=28.0' "$LOG" || fail "-crf 28 not applied (no crf=28.0 in encoder log)"

ABITRATE="$(ffprobe -v error -select_streams a:0 -show_entries stream=bit_rate \
	-of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; b=int(sys.argv[1]); sys.exit(0 if 80000 <= b <= 120000 else 1)' "$ABITRATE" ||
	fail "audio bitrate $ABITRATE not near the requested 96k"

echo "PASS: rc=0, crf=28 applied, audio bitrate=${ABITRATE}bps"
