#!/usr/bin/env bash
# TC-transcode-03: 滤镜链不被截断（AC-06）
# 判据：输出分辨率变为 640x480；drawtext 生效（带文字的输出与仅缩放的输出
# 内容不同，证明整条 -vf 链被完整传递并执行）。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media small.mp4 4 320x240)"
PLAIN="$TMP_ROOT/plain.mp4"
FILTERED="$TMP_ROOT/filtered.mp4"
LOG="$TMP_ROOT/cli.log"

"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -vf "scale=640:480" "$PLAIN" >/dev/null 2>&1

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 \
	-vf "scale=640:480,drawtext=text='acceptance':fontsize=32:fontcolor=white:x=20:y=20" \
	"$FILTERED" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "filter-chain transcode rc"

DIM="$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$FILTERED")"
[[ "$DIM" == "640,480" ]] || fail "resolution $DIM, want 640,480"
cmp -s "$PLAIN" "$FILTERED" && fail "drawtext produced no pixel change; filter chain truncated"
grep -q 'Error initializing filter' "$LOG" && fail "filter initialization error"

# 文本里的冒号是文本，不是协议地址：ffmpeg 对同一参数 rc=0 并出图，
# 判定不得把它拒成 400（C-7 原样透传 / AC-06）。
COLON="$TMP_ROOT/colon.mp4"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 \
	-vf "scale=640:480,drawtext=text='Chapter\\: 1':fontsize=32:fontcolor=white:x=20:y=20" \
	"$COLON" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "drawtext text with a colon rc"
cmp -s "$PLAIN" "$COLON" && fail "drawtext with colon text produced no pixel change; text rejected as a path"
grep -q 'Error initializing filter' "$LOG" && fail "filter initialization error for drawtext colon text"

# 滤镜实例名（filter@id）是合法 ffmpeg 语法：判据只查滤镜名，实例名原样透传。
INSTANCE="$TMP_ROOT/instance.mp4"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 \
	-vf "scale@0=640:480,drawtext@t=text='acceptance':fontsize=32" \
	"$INSTANCE" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "filter instance name rc"
DIM_INST="$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$INSTANCE")"
[[ "$DIM_INST" == "640,480" ]] || fail "instance-name filter produced resolution $DIM_INST, want 640,480"
cmp -s "$PLAIN" "$INSTANCE" && fail "instance-name filter chain produced no pixel change"
grep -q 'Error initializing filter' "$LOG" && fail "filter initialization error for filter instance names"

echo "PASS: rc=0, resolution=$DIM, drawtext visibly applied, drawtext text with a colon accepted, filter instance names accepted"
