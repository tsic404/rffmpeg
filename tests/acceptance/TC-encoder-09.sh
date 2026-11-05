#!/usr/bin/env bash
# TC-encoder-09: `=` 写法与多选项（AC-23、负向）
# 判据：-c:v=… 与 -vcodec=… 不构成已指定、不触发自动选择：无自动选择行、
# 改写链行 Rewritten 不含视频编码器；命令原样执行，ffmpeg 因该选项报错，
# rc≠0，且失败分类不落 ENCODER_UNSUPPORTED。同一命令先后给出两个 -c:v 时
# 以最后一个值为已指定：无自动选择行，输出编码器为该值。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media_av inline.mp4 3)"

# check_inline_rejected asserts one "=" spelling is not a selection: no
# auto-selection line, no rewritten encoder, ffmpeg rejects the option it was
# handed verbatim, and the failure is not classified ENCODER_UNSUPPORTED.
check_inline_rejected() {
	local label="$1" log="$2" rc="$3"
	[[ "$rc" -ne 0 ]] || fail "$label: rc=0, want a non-zero exit"
	grep -qE '^\[rffmpeg\] auto-selected ' "$log" &&
		fail "$label: auto-selection line present for an = joined option"
	grep -E '^\[rffmpeg\] .*Worker capabilities: .*Rewritten:' "$log" &&
		fail "$label: rewrite chain line rewrites a video encoder"
	grep -qE 'Unrecognized option|At least one output file must be specified' "$log" ||
		fail "$label: ffmpeg did not reject the = joined option"
	local job_id failure_type
	job_id="$(job_id_from_log "$log")"
	[[ -n "$job_id" ]] || fail "$label: job id missing from the CLI log"
	failure_type="$(job_field "$job_id" failure_type)"
	[[ "$failure_type" != "ENCODER_UNSUPPORTED" ]] ||
		fail "$label: failure classified ENCODER_UNSUPPORTED"
	return 0
}

run_inline() {
	local label="$1" arg="$2"
	local log="$TMP_ROOT/$label.log"
	set +e
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$MEDIA" -t 1 "$arg" "$TMP_ROOT/$label.mp4" 2>"$log"
	local rc=$?
	set -e
	check_inline_rejected "$label" "$log" "$rc"
}

run_inline c-v-equals "-c:v=libx264"
run_inline vcodec-equals "-vcodec=libx264"

# 同一命令两个 -c:v：以最后一个（生效者）为准直通，不触发自动选择。
LOG="$TMP_ROOT/last-wins.log"
OUT="$TMP_ROOT/last-wins.mp4"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -t 1 -c:v libx264 -c:v libx265 "$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "last -c:v wins rc"
[[ -f "$OUT" ]] || fail "last -c:v wins: output missing"
grep -qE '^\[rffmpeg\] auto-selected ' "$LOG" &&
	fail "last -c:v wins: auto-selection line present"
CODEC="$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of default=nw=1:nk=1 "$OUT")"
[[ "$CODEC" == "hevc" ]] || fail "last -c:v wins: output video codec is $CODEC, want hevc"

echo "PASS: = joined options are rejected by ffmpeg; the last -c:v decides"
