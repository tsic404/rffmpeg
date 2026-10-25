#!/usr/bin/env bash
# TC-selfheal-04: 慢 Worker 窗口外自动恢复不可达（AC-45，负向）
# 判据：经吞吐注入制造的慢 Worker 被驱逐后不再获得新作业、在跑作业结束后上报吞吐
# 归零；随后重启 Server，经过至少两个巡检 tick 与一个心跳周期，面板驱逐标记保持为
# 真、该 Worker 不被派单、标记不因 Server 重启或记录保留时长而消失。
# 窗口内的自动恢复不属本用例，不作断言。
#
# 编排要点：
# - RFFMPEG_THROUGHPUT_SCALE 把 ws 的上报吞吐压到 5%：完成量照常上报（不触发
#   busy 豁免），EWMA 却必然低于中位数；wf 用同一编码器正常上报，充当样本池的另
#   一半。两个样本即可判定，比值口径稳定命中。
# - 长作业在 wf 注册完成、仍低于 warmup 门槛时落到 ws：此时样本池只有 ws 一个成
#   员，不构成判定，落点失败可安全取消重投；长作业的计时因此不覆盖 wf 的注册开销
#   （冷机可达数十秒），「开始编码 → 驱逐」只隔 wf 的余下 warm 完成与一两个巡检
#   tick。
# - 拉取路径按容量派单、不看编码器清单，落点是一次竞争：先给 wf 的唯一槽位投一个
#   持有作业，再把长作业投给 ws——忙着的 wf 既不会被调度器派单也拉不走新的
#   pending。未落在目标 Worker 的尝试被取消重投，最多 8 次；这段窗口内没有可判定
#   的样本池，重投不会改变任何驱逐状态。
# - 素材 180s（本机 veryslow 单线程约 60s），第 5 段的等待预算按它设定；用例在长
#   作业结束时核对「驱逐 → 结束」的余量，不足 LONG_SURVIVAL_MARGIN_S 即判余量不
#   成立，而不是把「仍在跑」押在机器速度上。
# - 巡检 tick 2s、心跳 2s、离线保留 8s：两个 tick 加一个心跳（6s）与一次保留清理
#   都在观察窗口内到期，用例不依赖默认的 30s / 90s / 10m。
# - 重启前先停掉慢节点进程：活着的进程会在拉取冲突退避到期后重新注册，记录因此
#   始终新鲜，离线保留清理根本作用不到它，判据就失去意义；进程不在时记录停在最后
#   一次心跳，保留清理正是以那一刻起算，标记必须比它活得更久。
set -euo pipefail
PORT="${PORT:-18999}"
TICK_S=2
HEARTBEAT_S=2
RECORD_RETENTION_S=8
LONG_JOB_WAIT_S=300
LONG_SURVIVAL_MARGIN_S=10
# The post-restart observation must span at least two patrol ticks and one
# heartbeat period, and long enough for the retention sweep to have run.
POST_RESTART_OBSERVE_S=$((2 * TICK_S + HEARTBEAT_S + RECORD_RETENTION_S))
export RFFMPEG_THROUGHPUT_SCALE="ws=0.05"
SERVER_EXTRA_FLAGS="--worker-health-check-interval ${TICK_S}s --worker-heartbeat-timeout 20s --worker-offline-threshold ${RECORD_RETENTION_S}s --schedule-interval 1s"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
wait_server_ready

LIBX264_CONFIG="$TMP_ROOT/libx264-worker.json"
cat >"$LIBX264_CONFIG" <<'JSON'
{
  "auto_detect_codecs": false,
  "manual_encoders": ["libx264"],
  "manual_decoders": ["h264"]
}
JSON

SHORT="$(make_media sel-short.mp4 1 320x240)"
LONG="$(make_media sel-long.mp4 180 640x480)"

upload_file() {
	local log="$TMP_ROOT/upload-$(basename "$1").log"
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		probe "$1" >/dev/null 2>"$log"
	grep -oE 'Uploaded: .* -> [0-9a-f]+' "$log" | awk '{print $NF}' || true
}

FILE_SHORT="$(upload_file "$SHORT")"
[[ -n "$FILE_SHORT" ]] || fail "short input file id not captured"
FILE_LONG="$(upload_file "$LONG")"
[[ -n "$FILE_LONG" ]] || fail "long input file id not captured"

submit_job() {
	local name="$1" input="$2" preset="${3:-medium}" threads="${4:-1}" encoder="${5:-libx264}"
	local media_args resp code attempt
	media_args="[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"$encoder\",\"-preset\",\"$preset\",\"-threads\",\"$threads\",\"-y\"]"
	for attempt in $(seq 1 180); do
		resp="$(curl -sS -X POST -H "Authorization: Bearer $AUTH_TOKEN" \
			-H 'Content-Type: application/json' \
			-d "{\"input_files\":[\"$input\"],\"args\":$media_args,\"output_filename\":\"$name.mp4\"}" \
			-w '\n%{http_code}' \
			"$(acceptance_server_url)/api/v1/jobs")"
		code="${resp##*$'\n'}"
		resp="${resp%$'\n'*}"
		if [[ "$code" == "200" ]]; then
			python3 -c 'import json,sys; print(json.loads(sys.argv[1])["job_id"])' "$resp"
			return 0
		fi
		sleep 1
	done
	fail "job submission kept failing (last HTTP $code)"
}

worker_stats() {
	api_get /api/v1/workers |
		python3 -c '
import json, sys
data = json.load(sys.stdin)
out = {"median": data.get("cluster_median_throughput", 0), "workers": {}}
for w in data["workers"]:
    health = w.get("health") or {}
    out["workers"][w["name"]] = {
        "status": w.get("status"),
        "evicted": w.get("evicted"),
        "last_heartbeat": w.get("last_heartbeat"),
        "completed": health.get("completed_jobs", 0),
        "jobs_per_sec": health.get("jobs_per_sec", 0),
        "ewma": health.get("ewma_jobs_per_sec", 0),
        "active": len(health.get("active_jobs") or []),
    }
print(json.dumps(out))
'
}

worker_completed() {
	worker_stats | python3 -c '
import json, sys
print((json.loads(sys.stdin.read())["workers"].get(sys.argv[1]) or {}).get("completed", 0))
' "$1"
}

# require_ws_evicted LABEL fails unless the panel still lists ws with the
# eviction mark on. A missing record is a failure, never a skip: losing the
# record is exactly the way the mark disappears.
require_ws_evicted() {
	local stats
	stats="$(worker_stats)" || fail "worker panel unavailable $1"
	python3 -c '
import json, sys
s = json.loads(sys.argv[1])
w = s["workers"].get("ws")
if w is None:
    sys.exit("ws is missing from the worker panel %s" % sys.argv[2])
if w.get("evicted") is not True:
    sys.exit("ws eviction mark was cleared %s: %r" % (sys.argv[2], w))
' "$stats" "$1" || fail "eviction mark not held $1"
}

# ws_evicted reports whether the panel currently holds the eviction mark.
ws_evicted() {
	worker_stats | python3 -c '
import json, sys
w = (json.loads(sys.stdin.read())["workers"].get("ws") or {})
sys.exit(0 if w.get("evicted") is True else 1)
'
}

# hold_eviction LABEL SECONDS keeps asserting the mark for the whole window.
hold_eviction() {
	local label="$1" until=$((SECONDS + $2))
	while :; do
		require_ws_evicted "$label"
		(( SECONDS < until )) || return 0
		sleep 0.5
	done
}

in_flight_count() {
	api_get /api/v1/jobs | python3 -c '
import json, sys
jobs = json.load(sys.stdin)["jobs"]
print(sum(1 for j in jobs if j["status"] in ("pending", "queued", "running")))
'
}

ensure_no_ws_jobs() {
	api_get /api/v1/jobs | python3 -c '
import json, sys
jobs = json.load(sys.stdin)["jobs"]
bad = [(j.get("output_filename"), j["status"]) for j in jobs
       if j.get("worker_name") == "ws" and j["status"] not in ("completed", "failed", "cancelled")]
if bad:
    sys.exit("jobs still in flight on the evicted worker: %r" % bad)
' || fail "$1"
}

FLOW_N=0
feed_job() {
	FLOW_N=$((FLOW_N + 1))
	submit_job "flow-$FLOW_N" "$FILE_SHORT" ultrafast 1
}

# fill_pipeline keeps a small backlog so both workers stay busy without
# letting pending work pile up on whichever worker the dispatcher prefers.
fill_pipeline() {
	if (( $(in_flight_count) < 4 )); then
		feed_job >/dev/null
	fi
}

# place_job_on NAME LABEL INPUT PRESET THREADS submits a job and returns its id
# only once NAME has claimed it. The pull path claims pending jobs for whichever
# worker polls first without consulting its encoder list, so an attempt can be
# handed to the peer; that attempt is cancelled and resubmitted. Safe here
# because the peer is still below the warmup gate, so no judgement can run and a
# cancelled attempt changes nothing.
place_job_on() {
	local name="$1" label="$2" input="$3" preset="$4" threads="$5"
	local attempt job worker status deadline
	for attempt in $(seq 1 8); do
		job="$(submit_job "$label-$attempt" "$input" "$preset" "$threads")"
		deadline=$((SECONDS + 20))
		worker=""
		while :; do
			status="$(job_field "$job" status 2>/dev/null || true)"
			case "$status" in completed|failed|cancelled) break ;; esac
			worker="$(job_field "$job" worker_name 2>/dev/null || true)"
			if [[ -n "$worker" && "$worker" != "None" ]]; then break; fi
			(( SECONDS < deadline )) || break
			sleep 0.2
		done
		if [[ "$worker" == "$name" ]]; then
			printf '%s' "$job"
			return 0
		fi
		curl -sS -X DELETE -H "Authorization: Bearer $AUTH_TOKEN" \
			"$(acceptance_server_url)/api/v1/jobs/$job" >/dev/null || true
		wait_job_status "$job" 60 "cancelled completed failed" >/dev/null || true
	done
	fail "could not place the $label job on $name after 8 attempts"
}

# Phase 1: warm ws past the >=5 gate while it is the only libx264 worker, so
# every completion is deterministic.
start_worker ws RFFMPEG_CONFIG="$LIBX264_CONFIG" \
	RFFMPEG_HEARTBEAT_INTERVAL="${HEARTBEAT_S}s" RFFMPEG_POLL_INTERVAL=300ms \
	RFFMPEG_MAX_CONCURRENT=2
wait_worker ws
for i in $(seq 1 7); do submit_job "ws-warm-$i" "$FILE_SHORT" ultrafast 1 >/dev/null; done
DEADLINE=$((SECONDS + 120))
until (( $(worker_completed ws) >= 5 )); do
	(( SECONDS < DEADLINE )) || fail "slow worker never completed 5 warmup jobs"
	sleep 0.5
done

# Phase 2: bring the fast peer in. Its registration cost is paid here, before
# the long job exists — that is what keeps the park-to-eviction window short.
start_worker wf RFFMPEG_CONFIG="$LIBX264_CONFIG" \
	RFFMPEG_HEARTBEAT_INTERVAL="${HEARTBEAT_S}s" RFFMPEG_POLL_INTERVAL=300ms \
	RFFMPEG_MAX_CONCURRENT=1
wait_worker wf

# Phase 3: park the long job on ws while wf is still below the warmup gate.
# Only ws is eligible for judgement at this point, so the median sample pool has
# one member, no judgement runs, and an attempt that lands on the peer can be
# cancelled without touching the eviction state. The peer's single slot is held
# first: a busy peer is neither dispatched to (idle workers only) nor able to
# claim pending work (running+queued bound), which leaves ws as the only landing
# spot for the parked job.
HOLDER_JOB="$(place_job_on wf hold "$FILE_LONG" medium 1)"
LONG_JOB="$(place_job_on ws parked "$FILE_LONG" veryslow 1)"
wait_job_status "$LONG_JOB" 60 "running" >/dev/null || fail "parked job never started"
[[ "$(job_field "$LONG_JOB" worker_name)" == "ws" ]] || fail "parked job did not land on ws"
LONG_STARTED_AT=$SECONDS

# Phase 4: keep short jobs flowing so ws keeps producing while the parked job
# holds one of its two slots. ws can only reach the median pool on a fresh
# non-zero sample, and the peer's completions set the median it is measured
# against; the eviction follows once wf is past the gate.
DEADLINE=$((SECONDS + 240))
while :; do
	if ws_evicted; then break; fi
	(( SECONDS < DEADLINE )) || fail "slow worker was never evicted; panel=$(worker_stats)"
	fill_pipeline
	sleep 0.3
done
EVICTION_AT=$SECONDS
WS_COMPLETED_AT_EVICTION="$(worker_completed ws)"
(( WS_COMPLETED_AT_EVICTION >= 5 )) || fail "ws was judged before passing the warmup gate"

# Phase 5: the mark must not interrupt the parked job, and no new job may land
# on ws afterwards. The eviction stopped ws's pulls, so the probe can only run
# on the peer.
(( EVICTION_AT > LONG_STARTED_AT )) || fail "eviction was observed before the parked job started"
[[ "$(job_field "$LONG_JOB" status)" == "running" ]] ||
	fail "parked job no longer running at eviction; the window outgrew the source length"
require_ws_evicted "at the eviction instant"
FLOW_N=$((FLOW_N + 1))
POST_EVICT_JOB="$(submit_job "post-evict-$FLOW_N" "$FILE_SHORT" ultrafast 1)"
wait_job_status "$POST_EVICT_JOB" 120 completed >/dev/null ||
	fail "post-eviction job never completed"
[[ "$(job_field "$POST_EVICT_JOB" worker_name)" != "ws" ]] ||
	fail "a job submitted after eviction was assigned to the evicted worker"

DEADLINE=$((SECONDS + LONG_JOB_WAIT_S))
while :; do
	LONG_STATUS="$(job_field "$LONG_JOB" status 2>/dev/null || true)"
	if [[ "$LONG_STATUS" == "completed" ]]; then break; fi
	[[ "$LONG_STATUS" == "running" || "$LONG_STATUS" == "queued" ]] ||
		fail "the parked job ended as $LONG_STATUS instead of completing"
	(( SECONDS < DEADLINE )) ||
		fail "the parked job did not finish within ${LONG_JOB_WAIT_S}s; raise the source length and this budget together"
	require_ws_evicted "while the parked job ran to completion"
	sleep 0.5
done
LONG_DONE_AT=$SECONDS
# The margin is measured, not assumed: the parked job has to outlive the
# eviction observation by a real margin, otherwise "still running" was luck.
LONG_MARGIN=$((LONG_DONE_AT - EVICTION_AT))
(( LONG_MARGIN >= LONG_SURVIVAL_MARGIN_S )) ||
	fail "the parked job survived the eviction by only ${LONG_MARGIN}s (want >= ${LONG_SURVIVAL_MARGIN_S}s); the source length no longer covers the warmup window"
ensure_no_ws_jobs "jobs were still running on the evicted worker after its mark"

# Phase 6: with no work left, ws reports zero throughput on its next heartbeat
# — the window in which an automatic recovery could have happened is over.
DEADLINE=$((SECONDS + 60))
until worker_stats | python3 -c '
import json, sys
w = (json.loads(sys.stdin.read())["workers"].get("ws") or {})
sys.exit(0 if w.get("active") == 0 and w.get("jobs_per_sec") == 0 else 1)
'; do
	(( SECONDS < DEADLINE )) || fail "evicted worker never reported zero throughput; panel=$(worker_stats)"
	require_ws_evicted "while ws settled to zero throughput"
	sleep 0.5
done
WS_COMPLETED_SETTLED="$(worker_completed ws)"
(( WS_COMPLETED_SETTLED >= WS_COMPLETED_AT_EVICTION )) || fail "ws completion counter went backwards"
hold_eviction "across two patrol ticks after the throughput reached zero" $((2 * TICK_S))

# Take the slow node down before the restart. A live process keeps pulling, and
# its conflict backoff eventually re-registers it — the record then carries a
# fresh heartbeat and the retention sweep can never apply, which is what the
# criterion is about. With the process gone the record can only be kept alive by
# the eviction exemption.
WS_PID="$(worker_pid ws)"
[[ -n "$WS_PID" ]] || fail "ws worker pid was not recorded"
kill "$WS_PID" 2>/dev/null || true

# Phase 7: restart the Server. The mark lives in the database, but every worker
# is marked offline by the recovery pass, so the panel and the dispatch path
# have to keep the mark from the persisted state alone.
stop_server
start_server
RESTART_AT=$SECONDS
wait_server_ready
require_ws_evicted "immediately after the server restart"

# The peer comes back on its own (offline is recoverable by re-registering);
# the marked worker must not. Wait for the peer so the post-restart submission
# has somewhere to land, and confirm ws did not take the same path.
DEADLINE=$((SECONDS + 60))
until worker_stats | python3 -c '
import json, sys
w = (json.loads(sys.stdin.read())["workers"].get("wf") or {})
sys.exit(0 if w.get("status") != "offline" and w.get("evicted") is False else 1)
'; do
	(( SECONDS < DEADLINE )) || fail "peer worker did not come back after the restart; panel=$(worker_stats)"
	require_ws_evicted "while the peer re-registered"
	sleep 0.5
done

FLOW_N=$((FLOW_N + 1))
POST_RESTART_JOB="$(submit_job "post-restart-$FLOW_N" "$FILE_SHORT" ultrafast 1)"
wait_job_status "$POST_RESTART_JOB" 120 completed >/dev/null ||
	fail "post-restart job never completed"
[[ "$(job_field "$POST_RESTART_JOB" worker_name)" != "ws" ]] ||
	fail "the evicted worker was dispatched a job after the server restart"

# Phase 8: hold the observation for longer than the retention window, so both
# a missing mark and a retention sweep would be caught. ws's last heartbeat
# predates the restart (its heartbeats are refused while marked), which is what
# makes the record eligible for the offline sweep.
DEADLINE=$((RESTART_AT + POST_RESTART_OBSERVE_S))
while (( SECONDS < DEADLINE )); do
	require_ws_evicted "during the post-restart observation window"
	sleep 0.5
done
FINAL_STATS="$(worker_stats)"
python3 -c '
import datetime, json, sys
stats = json.loads(sys.argv[1])
retention = float(sys.argv[2])
w = stats["workers"].get("ws")
if w is None:
    sys.exit("ws record disappeared within the retention window")
stamp = datetime.datetime.fromisoformat(w["last_heartbeat"].replace("Z", "+00:00"))
age = (datetime.datetime.now(datetime.timezone.utc) - stamp).total_seconds()
if age <= retention:
    sys.exit("ws record was never eligible for the offline sweep (heartbeat age %.1fs <= %.1fs)" % (age, retention))
if w.get("evicted") is not True:
    sys.exit("ws eviction mark was lost to the offline retention sweep: %r" % w)
print("ws record survived %.1fs without a heartbeat (retention %.1fs)" % (age, retention))
' "$FINAL_STATS" "$RECORD_RETENTION_S" || fail "retention assertion failed"

echo "PASS: slow worker evicted, in-flight job completed ${LONG_MARGIN}s after the eviction was observed, throughput zeroed, mark held across restart and the ${RECORD_RETENTION_S}s record retention window without a single dispatch"
