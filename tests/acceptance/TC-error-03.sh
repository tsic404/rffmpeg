#!/usr/bin/env bash
# TC-error-03: 请求不存在的编码器（AC-13）
# 判据：提交成功（HTTP 200 + 作业标识）并落库 failure_type=ENCODER_UNAVAILABLE；
# CLI 轮询到终态 failed、rc≠0、stderr 逐字匹配。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media enc.mp4 2)"
CODEC="totes_not_a_codec"
LOG="$TMP_ROOT/cli.log"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v "$CODEC" "$TMP_ROOT/out.mp4" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 1 "unavailable-encoder rc"

JOB="$(job_id_from_log "$LOG")"
[[ -n "$JOB" ]] || fail "submission did not return a job id (HTTP 200 + id expected)"

WANT="Job failed: [ENCODER_UNAVAILABLE] No worker available with encoder: $CODEC (or compatible encoders)"
grep -qxF "$WANT" "$LOG" || fail "stderr line mismatch; want exactly: $WANT"

api_get "/api/v1/jobs/$JOB" |
	python3 -c '
import json, sys
job = json.load(sys.stdin)["job"]
assert job["failure_type"] == "ENCODER_UNAVAILABLE", job["failure_type"]
assert job["retryable"] is False, job["retryable"]
' || fail "job record is not ENCODER_UNAVAILABLE/retryable=false"

echo "PASS: job $JOB persisted ENCODER_UNAVAILABLE, rc=1, exact stderr line"
