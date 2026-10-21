#!/usr/bin/env bash
# TC-transcode-08: 特殊字符（空格 / 引号）参数传递（AC-10）
# 判据：含空格与引号的路径和参数完整传递未被截断，输出可播放。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

SRC="$MEDIA_DIR/input with 'quotes'.mp4"
ffmpeg -loglevel error -y -f lavfi -i "testsrc=duration=4:size=320x240:rate=24" \
	-c:v libx264 -preset ultrafast -pix_fmt yuv420p "$SRC"
OUT="$MEDIA_DIR/output with \"double\" and 'single'.mp4"
TITLE="a b 'c' \"d\""

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$SRC" -c:v libx264 -metadata "title=$TITLE" "$OUT" 2>"$TMP_ROOT/cli.log"
RC=$?
set -e
expect_rc "$RC" 0 "special-character transcode rc"

[[ -f "$OUT" ]] || fail "output missing: $OUT"
GOT="$(ffprobe -v error -show_entries format_tags=title -of default=nw=1:nk=1 "$OUT")"
[[ "$GOT" == "$TITLE" ]] || fail "metadata title truncated: got '$GOT', want '$TITLE'"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 4.0) <= 0.1 else 1)' "$DUR" ||
	fail "unexpected duration: $DUR"

echo "PASS: rc=0, quoted paths and metadata preserved verbatim"
