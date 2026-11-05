#!/usr/bin/env bash
# TC-encoder-07: 显式编码器写法识别（AC-23）
# 判据：-c:v / -codec:v / -codec:v:0 / -vcodec / 值属视频编码器的通用 -c 与
# -codec 每种写法均无自动选择行、无注入，输出编码器为请求值（直通行按 C-74）；
# `-c:v copy` 与 `-c copy` 无自动选择行、无注入，输出视频流参数与输入一致。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media explicit.mp4 3)"

video_codec() {
	ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of default=nw=1:nk=1 "$1"
}

# check_explicit asserts one spelling is recognised as a user selection: no
# auto-selection line, no rewrite chain (nothing was injected), the C-74
# pass-through line, and the encoder the user asked for in the output.
check_explicit() {
	local label="$1" out="$2" log="$3" enc="$4" want="$5"
	[[ -f "$out" ]] || fail "$label: output missing"
	grep -qE '^\[rffmpeg\] auto-selected ' "$log" &&
		fail "$label: auto-selection line present for an explicit encoder"
	grep -E '^\[rffmpeg\] .*Worker capabilities: .*Rewritten:' "$log" &&
		fail "$label: encoder was rewritten although the user selected one"
	grep -qxF "[rffmpeg] $enc -> $enc (Pass through unchanged)" "$log" ||
		fail "$label: missing pass-through line for $enc"
	local got
	got="$(video_codec "$out")"
	[[ "$got" == "$want" ]] || fail "$label: output video codec is $got, want $want"
	return 0
}

run_explicit() {
	local label="$1" flag="$2" value="$3"
	local out="$TMP_ROOT/$label.mp4" log="$TMP_ROOT/$label.log"
	set +e
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$MEDIA" -t 1 "$flag" "$value" "$out" 2>"$log"
	local rc=$?
	set -e
	expect_rc "$rc" 0 "$label: rc"
	check_explicit "$label" "$out" "$log" "$value" h264
}

run_explicit c-v -c:v libx264
run_explicit codec-v -codec:v libx264
run_explicit codec-v-0 -codec:v:0 libx264
run_explicit vcodec -vcodec libx264
run_explicit generic-c -c libx264
run_explicit generic-codec -codec libx264

# 流复制：输出视频流参数与输入一致（复制不做编码，参数必须原样保留）。
INPUT_VIDEO_PARAMS="$(ffprobe -v error -select_streams v:0 \
	-show_entries stream=codec_name,width,height,pix_fmt,r_frame_rate,profile \
	-of default=nw=1 "$MEDIA")"

check_copy() {
	local label="$1" out="$2" log="$3"
	[[ -f "$out" ]] || fail "$label: output missing"
	grep -qE '^\[rffmpeg\] auto-selected ' "$log" &&
		fail "$label: auto-selection line present for a stream copy"
	grep -E '^\[rffmpeg\] .*Worker capabilities: .*Rewritten:' "$log" &&
		fail "$label: stream copy was rewritten"
	local got
	got="$(ffprobe -v error -select_streams v:0 \
		-show_entries stream=codec_name,width,height,pix_fmt,r_frame_rate,profile \
		-of default=nw=1 "$out")"
	[[ "$got" == "$INPUT_VIDEO_PARAMS" ]] ||
		fail "$label: output video parameters differ from input: $got"
	return 0
}

run_copy() {
	local label="$1" flag="$2"
	local out="$TMP_ROOT/$label.mp4" log="$TMP_ROOT/$label.log"
	set +e
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$MEDIA" -t 1 "$flag" copy "$out" 2>"$log"
	local rc=$?
	set -e
	expect_rc "$rc" 0 "$label: rc"
	check_copy "$label" "$out" "$log"
}

run_copy copy-c-v -c:v
run_copy copy-c -c

echo "PASS: every explicit encoder spelling is respected, copy stays a stream copy"
