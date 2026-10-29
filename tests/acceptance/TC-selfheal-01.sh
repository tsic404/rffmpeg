#!/usr/bin/env bash
# TC-selfheal-01: kill 执行中的 Worker → 迁移重跑（AC-43）
#
# 分段判据，失败按段归因，不断言端到端总壁钟：
# - 检测段：kill 到迁移事件写入 ≤ 心跳超时窗口 + 一个巡检 tick；被 kill 的 Worker 在
#   同一 tick 内翻 offline；事件含原 worker、原因 heartbeat_timeout 与重试次数。
# - 重排段：迁移事件时间到作业认领时间（claimed_at，pending → queued）之差 ≤ 一个
#   派发 tick；每作业目标回填为存活 Worker。
# - 恢复段：完整重编（stderr 有进度行、无缓存命中行）并完成，CLI rc=0，输出可 ffprobe。
#   认领到服务端开始之间的输入准备（C-92）与执行时长按 C-31 / C-43 判定，不设时限。
#
# 默认用生产默认窗口判定（90s 心跳超时 / 30s 巡检 / 5s 派发），这是 AC-43 的权威证据。
# SELFHEAL_WINDOW=fast 把前两个窗口压到 10s / 2s：只缩短等待，上限仍由本次配置的
# 窗口相加得出，契约判据不变。
set -euo pipefail
PORT="${PORT:-18999}"
DISPATCH_TICK_S=5

case "${SELFHEAL_WINDOW:-default}" in
default)
	# 不传窗口参数：这里测的就是默认配置本身。
	HEARTBEAT_TIMEOUT_S=90
	HEALTH_CHECK_INTERVAL_S=30
	;;
fast)
	HEARTBEAT_TIMEOUT_S=10
	HEALTH_CHECK_INTERVAL_S=2
	;;
*)
	echo "unknown SELFHEAL_WINDOW (want default|fast)" >&2
	exit 2
	;;
esac
if [[ "${SELFHEAL_WINDOW:-default}" == fast ]]; then
	SERVER_EXTRA_FLAGS="--worker-heartbeat-timeout 10s --worker-health-check-interval 2s"
fi
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

start_selfheal_worker() {
	if [[ "${SELFHEAL_WINDOW:-default}" == fast ]]; then
		start_worker "$1" RFFMPEG_HEARTBEAT_INTERVAL=2s
	else
		start_worker "$1"
	fi
}

build_binaries
start_server
start_selfheal_worker worker-a
wait_worker worker-a

MEDIA="$(make_media heal.mp4 120 1280x720)"
LOG="$TMP_ROOT/cli.log"
OUT="$TMP_ROOT/healed.mp4"

# 1800s 只是挂起进程的守卫，不是判据：AC-43 不对端到端总壁钟设上限。
( timeout 1800 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
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
[[ "$(job_field "$JOB" worker_name)" == "worker-a" ]] || fail "job did not start on worker-a"

start_selfheal_worker worker-b
wait_worker worker-b

KILL_EPOCH="$(python3 -c 'import time; print(time.time())')"
kill -9 "$(worker_pid worker-a)" || fail "worker-a pid not recorded"

# 检测时刻的快照：轮询在迁移事件刚写入时结束，此时作业还没有被认领，每作业 target
# 仍是占位 NULL（契约内的「事件已写、尚未认领」中间态）。这份快照只判检测段事实与
# 事件时间戳；落点与认领时间在完成后的快照上判。
MIGRATIONS="$TMP_ROOT/migrations.json"
has_migration_event() {
	api_get /api/v1/migrations >"$MIGRATIONS" && python3 - "$MIGRATIONS" "$JOB" <<'PY'
import json, sys

events = json.load(open(sys.argv[1]))["events"]
job_id = sys.argv[2]
sys.exit(0 if any(e.get("worker_name") == "worker-a" and job_id in (e.get("job_ids") or []) for e in events) else 1)
PY
}

# 检测段：事件在「心跳超时 + 一个巡检 tick」内写入；+20s 只给轮询和日志消费留余量，
# 实际数值由事件时间戳与 kill 时刻之差判定。
DEADLINE=$((SECONDS + HEARTBEAT_TIMEOUT_S + HEALTH_CHECK_INTERVAL_S + 20))
until has_migration_event; do
	(( SECONDS < DEADLINE )) ||
		fail "检测段: no migration event for ${JOB} within ${HEARTBEAT_TIMEOUT_S}s + ${HEALTH_CHECK_INTERVAL_S}s (+20s poll guard)"
	sleep 0.5
done

WORKER_STATE="$(api_get /api/v1/workers | python3 -c '
import json, sys
workers = [w for w in json.load(sys.stdin)["workers"] if w["name"] == "worker-a"]
print(workers[0]["status"] if workers else "removed")
')"
[[ "$WORKER_STATE" == "offline" || "$WORKER_STATE" == "removed" ]] ||
	fail "检测段: worker-a status=${WORKER_STATE} after the migration event, want offline"

set +e
wait "$CLI_PID"
RC=$?
set -e
expect_rc "$RC" 0 "恢复段: migrated job CLI rc"

# 完成后的同一时刻快照：target 在认领时才回填（调度分配 / worker 拉取），
# 重排段判据用检测时刻的事件时间戳，落点用这份快照。
api_get /api/v1/migrations >"$TMP_ROOT/migrations-final.json"
api_get "/api/v1/jobs/$JOB" >"$TMP_ROOT/job.json"
if ! python3 - "$MIGRATIONS" "$TMP_ROOT/migrations-final.json" "$TMP_ROOT/job.json" "$JOB" "$KILL_EPOCH" >"$TMP_ROOT/segments.txt" <<'PY'
import json, sys
from datetime import datetime

detect_path, final_path, job_path, job_id, kill_epoch = sys.argv[1:6]
kill_epoch = float(kill_epoch)


def ts(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def migration_event(path):
    data = json.load(open(path))
    assert set(data) == {"events"}, sorted(data)
    return next((e for e in data["events"]
                 if e.get("worker_name") == "worker-a" and job_id in (e.get("job_ids") or [])), None)


# 检测段事实只读检测时刻的快照。
detected = migration_event(detect_path)
assert detected is not None, f"no migration event for worker-a carrying {job_id}"
assert detected["reason"] == "heartbeat_timeout", detected["reason"]
assert detected["retry_count"] >= 1, detected["retry_count"]
assert detected["jobs_migrated"] >= 1, detected["jobs_migrated"]

# 落点读完成后的快照：target 在认领之后才回填，检测时刻读到 NULL 是合法中间态。
settled = migration_event(final_path)
assert settled is not None, f"migration event disappeared from the settled snapshot: {job_id}"
assert settled["id"] == detected["id"], (settled["id"], detected["id"])
targets = [t for t in settled.get("targets") or [] if t.get("job_id") == job_id]
assert targets, f"no per-job target for {job_id}"
assert targets[-1].get("target_worker_name") == "worker-b", targets[-1]

job = json.load(open(job_path))["job"]
assert job["status"] == "completed", job["status"]
assert job["worker_name"] == "worker-b", job["worker_name"]
assert "claimed_at" in job, "job object exposes no claimed_at (claim time)"
assert job.get("retry_count", 0) >= 1, job.get("retry_count")

event_ts = ts(detected["timestamp"])
print(round((event_ts - kill_epoch) * 1000), round((ts(job["claimed_at"]) - event_ts) * 1000))
PY
then
	fail "migration event / job contract violated"
fi
read -r DETECT_MS DISPATCH_MS <"$TMP_ROOT/segments.txt"

DETECT_BUDGET_MS=$(( (HEARTBEAT_TIMEOUT_S + HEALTH_CHECK_INTERVAL_S) * 1000 ))
(( DETECT_MS <= DETECT_BUDGET_MS )) ||
	fail "检测段: kill->event ${DETECT_MS}ms > ${DETECT_BUDGET_MS}ms (${HEARTBEAT_TIMEOUT_S}s + ${HEALTH_CHECK_INTERVAL_S}s)"
(( DISPATCH_MS >= 0 )) ||
	fail "重排段: claim (${DISPATCH_MS}ms) precedes the migration event"
(( DISPATCH_MS <= DISPATCH_TICK_S * 1000 )) ||
	fail "重排段: event->claim ${DISPATCH_MS}ms > ${DISPATCH_TICK_S}s"

grep -qE '^Progress: [0-9]+\.[0-9]+% \| ETA: ' "$LOG" ||
	fail "恢复段: no ffmpeg progress output on CLI stderr"
grep -qF '[rffmpeg] Cache hit:' "$LOG" &&
	fail "恢复段: migrated execution hit the cache, want a full re-encode"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")" ||
	fail "恢复段: output is not readable by ffprobe"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 120.0) <= 0.2 else 1)' "$DUR" ||
	fail "恢复段: migrated output duration $DUR"

E2E_MS="$(python3 -c 'import sys, time; print(round((time.time() - float(sys.argv[1])) * 1000))' "$KILL_EPOCH")"
echo "PASS[${SELFHEAL_WINDOW:-default}]: detect=${DETECT_MS}ms (<=${DETECT_BUDGET_MS}ms), dispatch=${DISPATCH_MS}ms (<=$((DISPATCH_TICK_S * 1000))ms), rc=0, full re-encode on worker-b, e2e=${E2E_MS}ms (informational)"
