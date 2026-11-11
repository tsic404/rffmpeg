#!/usr/bin/env bash
# TC-encoder-08: 目标族不可判定时不改写（AC-21）
# 判据：未指定编码器输出到 gif（默认视频编码器不在 C-70 已知族）时，作业按
# ffmpeg 默认结果完成，rc=0 且输出可 ffprobe；stderr 无自动选择行，改写链行
# Rewritten 不含视频编码器；该次运行的失败分类不落 ENCODER_UNSUPPORTED。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media_av undecidable.mp4 2)"
OUT="$TMP_ROOT/anim.gif"
LOG="$TMP_ROOT/cli.log"

set +e
"$RFFMPEG_BIN/rffmpeg" \
	-i "$MEDIA" -t 1 "$OUT" 2>"$LOG"
RC=$?
set -e

# 失败分类不落 ENCODER_UNSUPPORTED：作业未执行 ffmpeg 的终结错误才属于该分类，
# 族不可判定时 ffmpeg 必须按自身默认结果执行。
JOB_ID="$(job_id_from_log "$LOG")"
[[ -n "$JOB_ID" ]] || fail "job id missing from the CLI log"
FAILURE_TYPE="$(job_field "$JOB_ID" failure_type)"
[[ "$FAILURE_TYPE" != "ENCODER_UNSUPPORTED" ]] ||
	fail "job failed as ENCODER_UNSUPPORTED although ffmpeg owns the default"

expect_rc "$RC" 0 "gif transcode rc"
[[ -f "$OUT" ]] || fail "output missing"
FFPROBE="$TMP_ROOT/ffprobe.txt"
ffprobe -v error -show_entries stream=codec_name -of default=nw=1:nk=1 "$OUT" >"$FFPROBE" ||
	fail "output is not probeable"
[[ -n "$(cat "$FFPROBE")" ]] || fail "output has no stream"

grep -qE '^\[rffmpeg\] auto-selected ' "$LOG" &&
	fail "auto-selection line present although the target family is undecidable"
grep -E '^\[rffmpeg\] .*Worker capabilities: .*Rewritten:' "$LOG" &&
	fail "rewrite chain line rewrites a video encoder although the family is undecidable"

echo "PASS: undecidable target family leaves the command to ffmpeg"
