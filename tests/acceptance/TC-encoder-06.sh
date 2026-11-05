#!/usr/bin/env bash
# TC-encoder-06: 未指定编码器 + 不产生视频输出（AC-21）
# 判据：纯音频输出容器、-vn、-map 只选音频三种请求均作业成功，输出可 ffprobe
# 且不含视频流；stderr 无自动选择行；改写链行 Requested / Rewritten 两字段均
# 为空（C-74 空字段省略），不出现视频编码器名。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media_av no-video.mp4 3)"

# check_no_video_request asserts one case's observable result: the job
# succeeded, the output is probeable and carries no video stream, no
# auto-selection line appears, and no rewrite chain line carries the
# Requested / Rewritten segments (C-74 omits empty fields).
check_no_video_request() {
	local label="$1" out="$2" log="$3"
	[[ -f "$out" ]] || fail "$label: output missing"
	ffprobe -v error -show_entries format=format_name -of default=nw=1:nk=1 "$out" >/dev/null ||
		fail "$label: output is not probeable"
	local video_streams
	video_streams="$(ffprobe -v error -select_streams v -show_entries stream=index -of csv=p=0 "$out")"
	[[ -z "$video_streams" ]] || fail "$label: output carries video stream(s): $video_streams"
	grep -qE '^\[rffmpeg\] auto-selected ' "$log" &&
		fail "$label: auto-selection line present for a request without video output"
	grep -E '^\[rffmpeg\] .*Worker capabilities: .*(Requested:|Rewritten:)' "$log" &&
		fail "$label: rewrite chain line carries Requested/Rewritten for a request that rewrites nothing"
	return 0
}

run_case() {
	local label="$1" out="$2"
	shift 2
	local log="$TMP_ROOT/$label.log"
	set +e
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$MEDIA" -t 1 "$@" "$out" 2>"$log"
	local rc=$?
	set -e
	expect_rc "$rc" 0 "$label: rc"
	check_no_video_request "$label" "$out" "$log"
}

# 1) 纯音频输出容器：wav 不承载视频流，ffmpeg 不会为其选择视频编码器。
run_case audio-container "$TMP_ROOT/audio-container.wav"

# 2) -vn 显式排除视频。
run_case vn "$TMP_ROOT/vn.m4a" -vn

# 3) -map 只选音频流（容器本身承载视频）。
run_case map-audio "$TMP_ROOT/map-audio.m4a" -map 0:a

echo "PASS: no auto-selection or encoder rewrite for requests without video output"
