#!/usr/bin/env bash
# TC-selfheal-01: kill 执行中的 Worker → 迁移重跑（AC-43）
# 判据：心跳超时窗口内翻离线；作业重新入队并由存活 Worker 完成（rc=0、输出
# 可 ffprobe）；迁移事件含原 worker / 原因 / 重试次数；端到端 ≤ 300s。
set -euo pipefail
PORT="${PORT:-18999}"
SERVER_EXTRA_FLAGS="--worker-heartbeat-timeout 10s --worker-health-check-interval 2s"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker worker-a RFFMPEG_HEARTBEAT_INTERVAL=2s
wait_worker worker-a

MEDIA="$(make_media heal.mp4 120 1280x720)"
LOG="$TMP_ROOT/cli.log"
OUT="$TMP_ROOT/healed.mp4"

( timeout 300 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -preset veryslow "$OUT" >/dev/null 2>"$LOG" ) &
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

start_worker worker-b RFFMPEG_HEARTBEAT_INTERVAL=2s
wait_worker worker-b

KILL_NS=$(now_ns)
kill -9 "$(worker_pid worker-a)" || fail "worker-a pid not recorded"

# The worker must be marked offline within the heartbeat window (+ tick).
DEADLINE=$((SECONDS + 30))
until api_get /api/v1/workers | python3 -c '
import json, sys
workers = [w for w in json.load(sys.stdin)["workers"] if w["name"] == "worker-a"]
sys.exit(0 if not workers or workers[0]["status"] == "offline" else 1)
'; do
	(( SECONDS < DEADLINE )) || fail "worker-a stayed online past the heartbeat window"
	sleep 1
done
OFFLINE_MS=$(( ($(now_ns) - KILL_NS) / 1000000 ))
(( OFFLINE_MS <= 20000 )) || fail "offline detection took ${OFFLINE_MS}ms, want <= 20s"

set +e
wait "$CLI_PID"
RC=$?
set -e
expect_rc "$RC" 0 "migrated job rc"
E2E_MS=$(( ($(now_ns) - KILL_NS) / 1000000 ))
(( E2E_MS <= 300000 )) || fail "kill->completion took ${E2E_MS}ms, want <= 300s"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 120.0) <= 0.2 else 1)' "$DUR" ||
	fail "migrated output duration $DUR"

api_get /api/v1/jobs |
	python3 -c '
import json, sys
jid = sys.argv[1]
jobs = json.load(sys.stdin)["jobs"]
job = next((j for j in jobs if j["id"] == jid), None)
assert job is not None, "job not found"
assert job["status"] == "completed", job["status"]
assert job["worker_name"] == "worker-b", job["worker_name"]
' "$JOB" || fail "job did not complete on the surviving worker"

api_get /api/v1/migrations |
	python3 -c '
import json, sys
events = json.load(sys.stdin)["events"]
assert events, "no migration event recorded"
e = next((x for x in events if x.get("worker_name") == "worker-a"), None)
assert e is not None, "no migration event for worker-a: %r" % [x.get("worker_name") for x in events]
assert e["reason"] == "heartbeat_timeout", e["reason"]
assert e["retry_count"] >= 1, e["retry_count"]
assert e["job_ids"], "job_ids empty"
assert e["jobs_migrated"] >= 1, e["jobs_migrated"]
' || fail "migration event contract violated"

echo "PASS: offline=${OFFLINE_MS}ms, rc=0, failover to worker-b, e2e=${E2E_MS}ms"
