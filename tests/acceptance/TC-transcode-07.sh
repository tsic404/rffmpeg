#!/usr/bin/env bash
# TC-transcode-07: 非原生选项名混入转码调用（AC-09、F2 负向）
# 判据一：`--quiet` 混入——banner 照常出现、该 token 原样出现在作业参数里、
#         作业在 Worker 侧按 ffmpeg 未知选项失败（FFMPEG_ERROR）。
# 判据二：`--server <url>` 混入——url 不被当作 server 配置，而是成为第二个
#         输出候选，调用按 C-6 在提交前拒绝，不产生作业。
# 判据三：不出现 rffmpeg 自造的「未知 rffmpeg 选项」类拒绝。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media mix.mp4 4)"
LOG="$TMP_ROOT/cli.log"
OUT="$TMP_ROOT/passed-through.mp4"

job_count() {
	api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))'
}

# --- 判据一：`--quiet` 不被消费 -------------------------------------------
set +e
"$RFFMPEG_BIN/rffmpeg" -i "$MEDIA" -c:v libx264 -preset veryfast --quiet "$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 1 "--quiet mixed into a transcode must not report success"

# banner 照常出现：静默开关只由配置面决定，命令行 token 不改变它。
expect_in_file "$LOG" "Remote FFmpeg Client"
grep -qE '^Server: .+ \(.+\)$' "$LOG" ||
	fail "banner is missing the server URL/source line: $(grep -E '^Server:' "$LOG" || true)"

JOB="$(job_id_from_log "$LOG")"
[[ -n "$JOB" ]] || fail "no job was submitted: --quiet must reach the worker, not select quiet mode"

api_get "/api/v1/jobs/$JOB" | python3 -c '
import json, sys
args = json.load(sys.stdin)["job"]["args"]
if "--quiet" not in args:
    sys.exit("--quiet missing from job args: %r" % (args,))
' || fail "--quiet was stripped from the job arguments"

wait_job_status "$JOB" 180 "failed" >/dev/null || fail "job $JOB did not fail at the worker"
api_get "/api/v1/jobs/$JOB" | python3 -c '
import json, sys
job = json.load(sys.stdin)["job"]
if job.get("failure_type") != "FFMPEG_ERROR":
    sys.exit("failure_type = %r, want FFMPEG_ERROR" % job.get("failure_type"))
' || fail "worker did not classify the unknown option as FFMPEG_ERROR"
expect_in_file "$LOG" "Job failed: [FFMPEG_ERROR]"

# --- 判据二：`--server <url>` 不被消费 -------------------------------------
OTHER="$TMP_ROOT/second-output.mp4"
SERVER_LOG="$TMP_ROOT/cli-server.log"
BEFORE="$(job_count)"
set +e
"$RFFMPEG_BIN/rffmpeg" -i "$MEDIA" -c:v libx264 "$OTHER" \
	--server "$(acceptance_server_url)" 2>"$SERVER_LOG"
RC=$?
set -e
expect_rc "$RC" 1 "--server mixed in must be rejected before submission"
expect_in_file "$SERVER_LOG" "multiple output files are not supported"
expect_no_panic "$SERVER_LOG"
[[ ! -e "$OTHER" ]] || fail "--server run produced an output file"
[[ "$(job_count)" == "$BEFORE" ]] || fail "--server run created a job"

# --- 判据三：没有 rffmpeg 自造的未知选项拒绝 -------------------------------
grep -qE 'unknown rffmpeg option|invalid rffmpeg option|unrecognized rffmpeg' "$LOG" "$SERVER_LOG" &&
	fail "rffmpeg invented an option-namespace rejection"

echo "PASS: --quiet reached the worker and failed there as FFMPEG_ERROR; --server became a second output candidate"
