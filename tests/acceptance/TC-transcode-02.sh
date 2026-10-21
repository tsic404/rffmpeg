#!/usr/bin/env bash
# TC-transcode-02: 多输入合并（AC-05）
# 判据：不报错；输出同时含视频与音频流；时长由 -shortest 决定（取较短输入）。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

VIDEO="$(make_media vid.mp4 4)"
AUDIO="$(make_audio aud.m4a 5)"
OUT="$TMP_ROOT/merged.mp4"
LOG="$TMP_ROOT/cli.log"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$VIDEO" -i "$AUDIO" -c:v copy -c:a aac -shortest "$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "multi-input transcode rc"

STREAMS="$(ffprobe -v error -show_entries stream=codec_type,codec_name -of csv=p=0 "$OUT")"
grep -q '^h264,video$' <<<"$STREAMS" || fail "video stream missing: $STREAMS"
grep -q '^aac,audio$' <<<"$STREAMS" || fail "audio stream missing: $STREAMS"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 4.0) <= 0.2 else 1)' "$DUR" ||
	fail "duration $DUR not governed by -shortest (video 4s, audio 5s)"

echo "PASS: rc=0, video+audio streams present, shortest duration=$DUR"
