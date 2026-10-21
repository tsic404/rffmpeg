#!/usr/bin/env bash
# TC-error-07: 提交时有 worker、随后崩溃（AC-17，Server 侧 sweep 分支）
# 判据：迁回 pending 后 (120s, 150s] 内 fail；分类 WORKER_CRASH、stderr 逐字；
# 从提交到 CLI 返回失败的壁钟 ≤ 300s。使用默认心跳/巡检/饥饿阈值。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media sweep120.mp4 120 1280x720)"
LOG="$TMP_ROOT/cli.log"
OUT="$TMP_ROOT/out.mp4"
START=$(now_ns)

timeout 320 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -preset veryslow -threads 2 "$OUT" >/dev/null 2>"$LOG" &
CLI_PID=$!
trap 'kill "$CLI_PID" 2>/dev/null || true; acceptance_cleanup' EXIT

JOB=""
for _ in $(seq 1 120); do
	JOB="$(job_id_from_log "$LOG")"
	[[ -n "$JOB" ]] && break
	sleep 0.5
done
[[ -n "$JOB" ]] || fail "job id not reported"
wait_job_status "$JOB" 30 "running" >/dev/null || fail "job never reached running"

kill -9 "$(worker_pid "acceptance-worker-$PORT")" || fail "worker pid not recorded"

PENDING_AT=""
FAIL_AT=""
DEADLINE=$((SECONDS + 300))
while (( SECONDS < DEADLINE )); do
	STATUS="$(job_field "$JOB" status 2>/dev/null || true)"
	if [[ "$STATUS" == "pending" && -z "$PENDING_AT" ]]; then
		PENDING_AT=$(now_ns)
	fi
	if [[ "$STATUS" == "failed" ]]; then
		FAIL_AT=$(now_ns)
		break
	fi
	sleep 1
done
wait "$CLI_PID" 2>/dev/null || true

[[ -n "$PENDING_AT" ]] || fail "job was never migrated back to pending"
[[ -n "$FAIL_AT" ]] || fail "job did not fail within 300s"

P2F_MS=$(( (FAIL_AT - PENDING_AT) / 1000000 ))
TOTAL_MS=$(( (FAIL_AT - START) / 1000000 ))
(( P2F_MS > 119000 && P2F_MS <= 151000 )) ||
	fail "pending->failed took ${P2F_MS}ms, want (120s, 150s]"
(( TOTAL_MS <= 300000 )) || fail "submit->failure took ${TOTAL_MS}ms, want <= 300s"

api_get "/api/v1/jobs/$JOB" |
	python3 -c '
import json, sys
job = json.load(sys.stdin)["job"]
assert job["status"] == "failed", job["status"]
assert job["failure_type"] == "WORKER_CRASH", job["failure_type"]
assert job["retryable"] is True, job["retryable"]
' || fail "job record is not failed/WORKER_CRASH/retryable=true"

WANT="Job failed: [WORKER_CRASH] worker crashed (heartbeat timeout) and no surviving worker to take over the job"
grep -qxF "$WANT" "$LOG" || fail "stderr line mismatch; want exactly: $WANT"

echo "PASS: WORKER_CRASH, pending->failed=${P2F_MS}ms, total=${TOTAL_MS}ms"
