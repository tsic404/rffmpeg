#!/usr/bin/env bash
# TC-transcode-10: 重新附着由配置面给出作业标识（AC-54）
# 判据一：本次调用不提交新作业，作业列表无新增行；输出下载到缺省文件名或唯一
#         位置参数给出的路径，退出码 0。
# 判据二：位置参数多于一个，或出现参数面之外的 token（含能力查询、帮助、版本名）
#         时，提交前拒绝、退出码非零、不提交作业，错误行指出该 token。
# 判据三：该标识只写在配置文件层时不触发重新附着，调用按普通转码判定（无 -i
#         时按 C-6 拒绝，而不是附着既有作业）。
# 判据四：-y / -n 按 C-21：下载目标已存在时默认拒绝覆盖并给出与 ffmpeg 一致的
#         文案；-y 覆盖；-n 立即失败；两者同时给出即报错。
# 判据五：期限与退出码按 C-31 / C-32：作业在期限内完成退出 0；期限耗尽且作业
#         仍在运行时不取消作业、退出 2，并提示可再次附着。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

job_count() {
	api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))'
}
job_status() { job_field "$1" status; }

# --- 前置：一个已完成的作业及其输出 ----------------------------------------
MEDIA="$(make_media resume-src.mp4 2)"
SEED_DIR="$TMP_ROOT/seed"
mkdir -p "$SEED_DIR"
SEED_LOG="$TMP_ROOT/seed.log"
set +e
(cd "$SEED_DIR" && "$RFFMPEG_BIN/rffmpeg" -i "$MEDIA" -c:v libx264 -preset veryfast seed-out.mp4 2>"$SEED_LOG")
RC=$?
set -e
expect_rc "$RC" 0 "seed transcode rc"
JOB="$(job_id_from_log "$SEED_LOG")"
[[ -n "$JOB" ]] || fail "seed run reported no job id"
wait_job_status "$JOB" 180 "completed" >/dev/null || fail "seed job $JOB did not complete"

# --- 判据一：重新附着不提交新作业 ------------------------------------------
DL_DIR="$TMP_ROOT/download"
mkdir -p "$DL_DIR"
BEFORE="$(job_count)"
LOG="$TMP_ROOT/reattach.log"
set +e
(cd "$DL_DIR" && env RFFMPEG_JOB_ID="$JOB" "$RFFMPEG_BIN/rffmpeg" >/dev/null 2>"$LOG")
RC=$?
set -e
expect_rc "$RC" 0 "re-attach rc"
[[ -s "$DL_DIR/seed-out.mp4" ]] || fail "re-attach did not write the recorded output filename"
[[ "$(job_count)" == "$BEFORE" ]] || fail "re-attach submitted a new job"

# 唯一位置参数覆盖下载目标。
EXPLICIT="$TMP_ROOT/explicit.mp4"
set +e
(cd "$DL_DIR" && env RFFMPEG_JOB_ID="$JOB" "$RFFMPEG_BIN/rffmpeg" -y "$EXPLICIT" >/dev/null 2>"$TMP_ROOT/reattach-explicit.log")
RC=$?
set -e
expect_rc "$RC" 0 "re-attach explicit target rc"
[[ -s "$EXPLICIT" ]] || fail "explicit re-attach target missing"
[[ "$(job_count)" == "$BEFORE" ]] || fail "explicit re-attach submitted a new job"

# --- 判据二：参数越界在提交前拒绝并指出 token ------------------------------
reject_resume() {
	local label="$1" want="$2"
	shift 2
	local log="$TMP_ROOT/reattach-$label.log" rc=0 before
	before="$(job_count)"
	set +e
	(cd "$DL_DIR" && env RFFMPEG_JOB_ID="$JOB" "$RFFMPEG_BIN/rffmpeg" "$@" >/dev/null 2>"$log")
	rc=$?
	set -e
	expect_rc "$rc" 1 "re-attach rejection rc ($label)"
	expect_in_file "$log" "$want"
	expect_no_panic "$log"
	[[ "$(job_count)" == "$before" ]] || fail "rejection ($label) submitted a job"
}
reject_resume two-positions "at most one output path" a.mp4 b.mp4
reject_resume capability-query '"-encoders"' -encoders
reject_resume help '"-help"' -help
reject_resume version '"-version"' -version
reject_resume unknown-flag '"-i"' -i "$MEDIA"

# --- 判据三：配置文件层不承载该标识 ----------------------------------------
CFG_DIR="$TMP_ROOT/config-only"
mkdir -p "$CFG_DIR"
cat >"$CFG_DIR/rffmpeg.json" <<JSON
{"server_url":"$(acceptance_server_url)","token":"$AUTH_TOKEN","job_id":"$JOB"}
JSON
chmod 600 "$CFG_DIR/rffmpeg.json"
BEFORE="$(job_count)"
CFG_LOG="$TMP_ROOT/config-only.log"
set +e
(cd "$CFG_DIR" && env -u RFFMPEG_JOB_ID "$RFFMPEG_BIN/rffmpeg" -c:v libx264 >/dev/null 2>"$CFG_LOG")
RC=$?
set -e
expect_rc "$RC" 1 "config-file job id rc"
expect_in_file "$CFG_LOG" "no input file specified"
[[ "$(job_count)" == "$BEFORE" ]] || fail "config-file job id path submitted a job"

# --- 判据四：-y / -n 按 C-21 ------------------------------------------------
TARGET_DIR="$TMP_ROOT/overwrite"
mkdir -p "$TARGET_DIR"
cp "$DL_DIR/seed-out.mp4" "$TARGET_DIR/target.mp4"

set +e
(cd "$TARGET_DIR" && env RFFMPEG_JOB_ID="$JOB" "$RFFMPEG_BIN/rffmpeg" target.mp4 >/dev/null 2>"$TMP_ROOT/reattach-default.log")
RC=$?
set -e
expect_rc "$RC" 1 "default overwrite refusal rc"
expect_in_file "$TMP_ROOT/reattach-default.log" "Not overwriting - exiting"

set +e
(cd "$TARGET_DIR" && env RFFMPEG_JOB_ID="$JOB" "$RFFMPEG_BIN/rffmpeg" -n target.mp4 >/dev/null 2>"$TMP_ROOT/reattach-n.log")
RC=$?
set -e
expect_rc "$RC" 1 "-n overwrite rc"
expect_in_file "$TMP_ROOT/reattach-n.log" "already exists. Exiting."

set +e
(cd "$TARGET_DIR" && env RFFMPEG_JOB_ID="$JOB" "$RFFMPEG_BIN/rffmpeg" -y -n target.mp4 >/dev/null 2>"$TMP_ROOT/reattach-conflict.log")
RC=$?
set -e
expect_rc "$RC" 1 "-y -n conflict rc"
expect_in_file "$TMP_ROOT/reattach-conflict.log" "both -y and -n supplied"

set +e
(cd "$TARGET_DIR" && env RFFMPEG_JOB_ID="$JOB" "$RFFMPEG_BIN/rffmpeg" -y target.mp4 >/dev/null 2>"$TMP_ROOT/reattach-y.log")
RC=$?
set -e
expect_rc "$RC" 0 "-y overwrite rc"
[[ -s "$TARGET_DIR/target.mp4" ]] || fail "-y did not write the target"

# --- 判据五：期限耗尽不取消作业、退出 2 ------------------------------------
LONG="$MEDIA_DIR/resume-long.mp4"
if [[ ! -f "$LONG" ]]; then
	ffmpeg -loglevel error -y -f lavfi \
		-i testsrc=duration=120:size=1920x1080:rate=30 \
		-c:v libx264 "$LONG"
fi
LONG_LOG="$TMP_ROOT/reattach-long.log"
mkdir -p "$SEED_DIR"
set +e
(cd "$SEED_DIR" && "$RFFMPEG_BIN/rffmpeg" -i "$LONG" -c:v libx264 -preset veryslow long-out.mp4 >/dev/null 2>"$LONG_LOG") &
LONG_CLI_PID=$!
set -e
LONG_JOB=""
for _ in $(seq 1 120); do
	LONG_JOB="$(job_id_from_log "$LONG_LOG" 2>/dev/null || true)"
	[[ -n "$LONG_JOB" ]] && break
	sleep 0.5
done
[[ -n "$LONG_JOB" ]] || { kill "$LONG_CLI_PID" 2>/dev/null || true; fail "long job was never submitted"; }
wait_job_status "$LONG_JOB" 120 "running" >/dev/null || fail "long job never started running"

GIVEUP_LOG="$TMP_ROOT/reattach-giveup.log"
set +e
(cd "$DL_DIR" && env RFFMPEG_JOB_ID="$LONG_JOB" RFFMPEG_JOB_TIMEOUT=2s \
	"$RFFMPEG_BIN/rffmpeg" giveup-out.mp4 >/dev/null 2>"$GIVEUP_LOG")
RC=$?
set -e
expect_rc "$RC" 2 "deadline-exhausted re-attach rc"
expect_in_file "$GIVEUP_LOG" "RFFMPEG_JOB_ID=$LONG_JOB"
STATUS="$(job_status "$LONG_JOB")"
[[ "$STATUS" != "cancelled" ]] ||
	fail "re-attach cancelled the job: status=$STATUS"
[[ ! -e "$DL_DIR/giveup-out.mp4" ]] || fail "give-up run left a partial output"

# Cleanup: the long job and its submitting CLI are this test's own.
curl -fsS -X DELETE -H "Authorization: Bearer $AUTH_TOKEN" \
	"$(acceptance_server_url)/api/v1/jobs/$LONG_JOB" >/dev/null 2>&1 || true
kill "$LONG_CLI_PID" 2>/dev/null || true
wait "$LONG_CLI_PID" 2>/dev/null || true

echo "PASS: re-attach submits no job, downloads by recorded name, rejects out-of-surface tokens before submit, honours -y/-n, and gives up with rc=2 without cancelling"
