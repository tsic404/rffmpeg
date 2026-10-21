#!/usr/bin/env bash
# TC-transcode-07: rffmpeg 专属选项与 ffmpeg 选项混用（AC-09、F2 负向）
# 判据：专属选项被 CLI 消费（不出现在作业 args 中），ffmpeg 不因未知选项
# 报错，输出可播放。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media mix.mp4 4)"
OUT="$TMP_ROOT/mixed.mp4"
LOG="$TMP_ROOT/cli.log"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	--timeout 5m --poll-timeout 2m --max-retries 3 --auto-hw=false \
	-i "$MEDIA" -c:v libx264 -preset veryfast "$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "mixed-options transcode rc"

grep -qE 'Unrecognized option|Option .* not found|Error splitting the argument list' "$LOG" &&
	fail "ffmpeg received an rffmpeg-only option"

JOB="$(job_id_from_log "$LOG")"
[[ -n "$JOB" ]] || fail "job id not reported"
api_get "/api/v1/jobs/$JOB" |
	python3 -c '
import json, sys
args = json.load(sys.stdin)["job"]["args"]
leaked = [a for a in args if a.startswith("--")]
if leaked:
    sys.exit("rffmpeg-only options leaked into job args: %r" % leaked)
' || fail "rffmpeg-only option leaked into ffmpeg args"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 4.0) <= 0.1 else 1)' "$DUR" ||
	fail "unexpected duration: $DUR"

echo "PASS: rc=0, no rffmpeg-only options in job args, duration=$DUR"
