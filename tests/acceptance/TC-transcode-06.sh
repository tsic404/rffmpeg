#!/usr/bin/env bash
# TC-transcode-06: 从含音频流的源提取音频并转码（AC-07）
# 判据：不报错；输出只有音频流且可播放（mp3）。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media_av av.mp4 4)"
OUT="$TMP_ROOT/extracted.mp3"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -vn -c:a libmp3lame -b:a 128k "$OUT" 2>"$TMP_ROOT/cli.log"
RC=$?
set -e
expect_rc "$RC" 0 "audio-extraction rc"

STREAMS="$(ffprobe -v error -show_entries stream=codec_type,codec_name -of csv=p=0 "$OUT")"
grep -q '^mp3,audio$' <<<"$STREAMS" || fail "expected a single mp3 audio stream, got: $STREAMS"
grep -q ',video$' <<<"$STREAMS" && fail "video stream present despite -vn"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 4.0) <= 0.2 else 1)' "$DUR" ||
	fail "unexpected audio duration: $DUR"

echo "PASS: rc=0, mp3 audio-only output, duration=$DUR"
