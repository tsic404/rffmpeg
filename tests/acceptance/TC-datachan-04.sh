#!/usr/bin/env bash
# TC-datachan-04: 流式输出拒绝路径两侧（AC-35）
# 判据：裸管道与不可写管道 muxer 在提交前被拒（子串匹配）；可流式容器不得误拒。
# 另断言 stdout 别名归一：输出位置的裸 `pipe:` 与 `pipe:1` 同属流式输出。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media pipe.mp4 4)"

set +e
"$RFFMPEG_BIN/rffmpeg" \
	-i "$MEDIA" - >/dev/null 2>"$TMP_ROOT/bare.log"
BARE_RC=$?
"$RFFMPEG_BIN/rffmpeg" \
	-i "$MEDIA" -f m4a - >/dev/null 2>"$TMP_ROOT/m4a.log"
M4A_RC=$?
set -e

expect_rc "$BARE_RC" 1 "bare pipe rc"
expect_in_file "$TMP_ROOT/bare.log" "cannot auto-detect a muxer for a pipe"
expect_rc "$M4A_RC" 1 "m4a pipe rc"
expect_in_file "$TMP_ROOT/m4a.log" "this muxer cannot write to a pipe"

# Streamable containers must not be rejected.
set +e
"$RFFMPEG_BIN/rffmpeg" \
	-i "$MEDIA" -c:v libx264 -f mpegts - >"$TMP_ROOT/out.ts" 2>"$TMP_ROOT/ts.log"
TS_RC=$?
set -e
expect_rc "$TS_RC" 0 "mpegts streaming rc"
TS_FMT="$(ffprobe -v error -show_entries format=format_name -of default=nw=1:nk=1 "$TMP_ROOT/out.ts")"
grep -q 'mpegts' <<<"$TS_FMT" || fail "mpegts stream is not probeable: $TS_FMT"

# 输出位置的 stdout 别名：裸 `pipe:` 与 `pipe:1` 归一到同一个流式目标，可流式
# 容器照常产出字节，且不落一个名为 `pipe:` 的文件。
for ALIAS in "pipe:" "pipe:1"; do
	set +e
	"$RFFMPEG_BIN/rffmpeg" \
		-i "$MEDIA" -c:v libx264 -f mpegts "$ALIAS" >"$TMP_ROOT/alias.bin" 2>"$TMP_ROOT/alias-${ALIAS%:*}.log"
	ALIAS_RC=$?
	set -e
	expect_rc "$ALIAS_RC" 0 "stdout alias $ALIAS rc"
	[[ -s "$TMP_ROOT/alias.bin" ]] || fail "stdout alias $ALIAS produced no bytes"
	[[ ! -e "$TMP_ROOT/$ALIAS" ]] || fail "stdout alias $ALIAS created an output file"
done

# mp4 is corrected by the Worker's fragmented-movflags injection; must pass.
set +e
"$RFFMPEG_BIN/rffmpeg" \
	-i "$MEDIA" -c:v libx264 -f mp4 - >"$TMP_ROOT/out.mp4" 2>"$TMP_ROOT/mp4.log"
MP4_RC=$?
set -e
expect_rc "$MP4_RC" 0 "mp4 streaming rc"

echo "PASS: both rejections exact, mpegts/mp4 streams accepted"
