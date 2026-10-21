#!/usr/bin/env bash
# TC-datachan-07: > 100 MiB 输入端到端转码（AC-46）
# 判据：rc=0、输出可 ffprobe 且时长与输入一致、无上传失败类错误。E2E 不
# 区分单次/分片上传路径（设计不暴露该标记）。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

# 3s of 1280x720@30 rawvideo is ~124 MiB: uncompressed on purpose so the input
# exceeds the 100 MiB single-upload boundary regardless of codec efficiency.
BIG="$MEDIA_DIR/big-input.avi"
if [[ ! -f "$BIG" ]]; then
	ffmpeg -loglevel error -y -f lavfi \
		-i "testsrc=duration=3:size=1280x720:rate=30" \
		-c:v rawvideo -pix_fmt yuv420p "$BIG"
fi
SIZE="$(stat -c %s "$BIG")"
(( SIZE > 100 * 1024 * 1024 )) || fail "input only ${SIZE}B; need > 100 MiB"

LOG="$TMP_ROOT/cli.log"
OUT="$TMP_ROOT/big-out.mp4"
START=$(now_ns)
set +e
timeout 900 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$BIG" -c:v libx264 -preset veryfast "$OUT" 2>"$LOG"
RC=$?
set -e
WALL_MS=$(( ($(now_ns) - START) / 1000000 ))

expect_rc "$RC" 0 "large-input rc"
grep -qiE 'upload failed|chunk upload|resume failed' "$LOG" && fail "upload error reported: $(grep -iE 'upload failed' "$LOG" | head -1)"

IN_DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$BIG")"
OUT_DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - float(sys.argv[2])) <= 0.2 else 1)' "$OUT_DUR" "$IN_DUR" ||
	fail "output duration $OUT_DUR != input duration $IN_DUR"

echo "PASS: ${SIZE}B input transcoded in $((WALL_MS / 1000))s, duration match"