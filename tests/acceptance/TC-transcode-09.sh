#!/usr/bin/env bash
# TC-transcode-09: 与 ffmpeg 同名的单横线选项（画质 -q）透传（AC-52）
# 判据一：该次输出与原生 ffmpeg 同参数输出一致（AC-04 口径），画质取值生效、
#         未被 CLI 吞掉。
# 判据二：该次运行的 banner 照常出现；静默配置开启的那次不出现。
# 判据三：不产生解析错误、缺值报错或未知选项提示。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media quality.mp4 2)"
OUT="$TMP_ROOT/delivered.jpg"
NATIVE="$TMP_ROOT/native.jpg"
LOG="$TMP_ROOT/cli.log"
QUIET_LOG="$TMP_ROOT/cli-quiet.log"

# -q is ffmpeg's fixed-quality option and takes the value 2: a single mjpeg
# frame keeps the comparison byte-exact while making the quality visible.
ARGS=(-c:v mjpeg -q 2 -frames:v 1)

set +e
"$RFFMPEG_BIN/rffmpeg" -i "$MEDIA" "${ARGS[@]}" "$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "fixed-quality transcode rc"

# 判据三：没有解析错误。
grep -qE 'Error parsing arguments|requires a value|unknown option|Unrecognized option' "$LOG" &&
	fail "the CLI rejected or mangled the native -q option: $(cat "$LOG")"

# 判据二：非静默配置下 banner 照常出现。
expect_in_file "$LOG" "Remote FFmpeg Client"
grep -qE '^Server: .+ \(.+\)$' "$LOG" || fail "banner lacks the server URL/source line"

# 判据一：与原生 ffmpeg 同参数输出逐字节一致。
ffmpeg -loglevel error -y -i "$MEDIA" "${ARGS[@]}" "$NATIVE"
cmp -s "$OUT" "$NATIVE" || fail "output differs from native ffmpeg: -q 2 was not applied"
# 画质取值真的生效：另一个取值产出不同字节。
ffmpeg -loglevel error -y -i "$MEDIA" -c:v mjpeg -q 18 -frames:v 1 "$TMP_ROOT/low.jpg"
cmp -s "$OUT" "$TMP_ROOT/low.jpg" && fail "-q 2 and -q 18 produced identical bytes; the value was swallowed"

# 静默配置对照：banner 不出现。
set +e
RFFMPEG_QUIET=1 "$RFFMPEG_BIN/rffmpeg" -i "$MEDIA" "${ARGS[@]}" "$TMP_ROOT/quiet.jpg" 2>"$QUIET_LOG"
QUIET_RC=$?
set -e
expect_rc "$QUIET_RC" 0 "quiet-config transcode rc"
grep -q 'Remote FFmpeg Client' "$QUIET_LOG" && fail "quiet config still printed the banner"
grep -qE '^Server: ' "$QUIET_LOG" && fail "quiet config still printed the server line"

echo "PASS: -q 2 reached ffmpeg unchanged (byte-identical to native, distinguishable from -q 18); banner present only without quiet config"
