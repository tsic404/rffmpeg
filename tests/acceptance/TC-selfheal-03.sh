#!/usr/bin/env bash
# TC-selfheal-03: 慢 Worker 驱逐（AC-45）
# 判据：比值口径命中后标记驱逐且不再分配新作业；样本池不足两个样本时不构成
# 判定、标记保持；已有作业跑到终态；空闲 Worker 不误判；warmup 门槛生效。
# 恢复半段 E2E 不可达（设计已注明），由实现侧断言覆盖。
#
# 编排要点：
# - 最新心跳吞吐为 0 的忙碌 Worker 走 busy-exempt 分支，不产生完成量就永远
#   进不了比值判定；ws 用 veryslow 长作业占住一个槽位（验证驱逐不中断在跑
#   作业），另一个槽位持续吃短作业，使每次健康检查都有非零吞吐样本。
# - RFFMPEG_THROUGHPUT_SCALE 把 ws 的上报吞吐压到 5%：完成量照常上报（不
#   触发 busy-exempt），EWMA 却必然低于中位数，比值口径因此可稳定命中。
# - idle-worker 只声明 mpeg4，用 6 个 mpeg4 作业跨过 >=5 的 warmup 门槛。
#   拉取路径按状态认领 pending 作业、不按编码器过滤，所以它的拉取周期拉长
#   到 IDLE_POLL_INTERVAL：6 个作业先由调度器排为 queued，首次拉取一次性
#   交付（此时 activeCount=maxConcurrent，不再认领 pending），下一次拉取
#   远在观察窗口之后。它在观察期内真正空闲且 completed 冻结；同时具备
#   「已过 warmup 门槛」与「空闲」两个事实，唯一让它免于比值判定的就是
#   statetable 的空闲样本排除。
set -euo pipefail
PORT="${PORT:-18999}"
IDLE_POLL_INTERVAL=90s
export RFFMPEG_THROUGHPUT_SCALE="ws=0.05"
SERVER_EXTRA_FLAGS="--worker-health-check-interval 2s --worker-heartbeat-timeout 20s"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
wait_server_ready

# Phase 1: warm ws past the >=5 gate. It is the only worker advertising
# libx264 here, so every submission below is deterministic.
start_worker ws RFFMPEG_HEARTBEAT_INTERVAL=1s RFFMPEG_MAX_CONCURRENT=2
wait_worker ws

SHORT="$(make_media sel-short.mp4 1 320x240)"
LONG="$(make_media sel-long.mp4 180 1280x720)"
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$SHORT" -c:v libx264 "$TMP_ROOT/warm0.mp4" >/dev/null 2>"$TMP_ROOT/warm0.log"
FILE_ID="$(grep -oE 'Uploaded: .* -> [0-9a-f]+' "$TMP_ROOT/warm0.log" | awk '{print $NF}' || true)"
[[ -n "$FILE_ID" ]] || fail "short input file id not captured"

"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	probe "$LONG" >/dev/null 2>"$TMP_ROOT/long.log"
FILE_ID_LONG="$(grep -oE 'Uploaded: .* -> [0-9a-f]+' "$TMP_ROOT/long.log" | awk '{print $NF}' || true)"
[[ -n "$FILE_ID_LONG" ]] || fail "long input file id not captured"

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
        "evicted": w.get("evicted"),
        "completed": health.get("completed_jobs", 0),
        "jobs_per_sec": health.get("jobs_per_sec", 0),
        "ewma": health.get("ewma_jobs_per_sec", 0),
        "active": len(health.get("active_jobs") or []),
    }
print(json.dumps(out))
'
}

# worker_idle NAME MIN_COMPLETED -- true when the worker passed the warmup gate
# and reports zero latest throughput with no active jobs (the idle definition).
worker_idle() {
	python3 -c '
import json, sys
w = json.loads(sys.argv[1])["workers"].get(sys.argv[2], {})
sys.exit(0 if w.get("completed", 0) >= int(sys.argv[3])
         and w.get("active") == 0 and w.get("jobs_per_sec") == 0 else 1)
' "$1" "$2" "$3"
}

for i in $(seq 1 8); do submit_job "ws-warm-$i" "$FILE_ID" >/dev/null; done
DEADLINE=$((SECONDS + 90))
until worker_idle "$(worker_stats)" ws 5; do
	(( SECONDS < DEADLINE )) || fail "slow worker never completed 5 warmup jobs"
	sleep 1
done

# Phase 2: park the long job on ws (still the only libx264 worker). It occupies
# one of ws's two slots until it finishes on its own, so the eviction must not
# interrupt it; while it runs, ws is also a permanent member of the median
# sample pool (a worker with active jobs is sampled even at zero throughput).
LONG_JOB="$(submit_job sel-long "$FILE_ID_LONG" veryslow 1)"
wait_job_status "$LONG_JOB" 30 "running" >/dev/null || fail "long job never ran"
[[ "$(job_field "$LONG_JOB" worker_name)" == "ws" ]] || fail "long job did not land on ws"

# Phase 3: register the idle worker with a long pull interval. Its mpeg4-only
# encoder list keeps the scheduler from ever handing it libx264 work; with ws
# busy on the parked job, the six mpeg4 jobs are all queued to it (fewest
# completed wins the tie) and are delivered together at the first pull, when
# activeCount == maxConcurrent and the pull therefore claims no pending job.
IDLE_CONFIG="$TMP_ROOT/idle-worker.json"
cat >"$IDLE_CONFIG" <<'JSON'
{
  "auto_detect_codecs": false,
  "manual_encoders": ["mpeg4"],
  "manual_decoders": ["h264"]
}
JSON
start_worker idle-worker RFFMPEG_CONFIG="$IDLE_CONFIG" \
	RFFMPEG_HEARTBEAT_INTERVAL=1s RFFMPEG_POLL_INTERVAL="$IDLE_POLL_INTERVAL" \
	RFFMPEG_MAX_CONCURRENT=6
wait_worker idle-worker
for i in $(seq 1 6); do submit_job "idle-warm-$i" "$FILE_ID" medium 1 mpeg4 >/dev/null; done

# Phase 4: bring the fast worker in and warm it past the >=5 gate. Its short
# jobs are the high cluster-median sample ws is judged against.
start_worker wf RFFMPEG_HEARTBEAT_INTERVAL=1s RFFMPEG_MAX_CONCURRENT=1
wait_worker wf
DEADLINE=$((SECONDS + 120))
i=0
until python3 -c 'import json,sys; sys.exit(0 if json.loads(sys.argv[1])["workers"].get("wf",{}).get("completed",0) >= 5 else 1)' "$(worker_stats)"; do
	(( SECONDS < DEADLINE )) || fail "fast worker never completed 5 warmup jobs"
	i=$((i + 1))
	submit_job "wf-warm-$i" "$FILE_ID" >/dev/null
	sleep 0.2
done

# Phase 5: wait for the idle worker's first pull to deliver its six queued
# jobs, then let the warmup EWMA decay. The warmup load can already have marked
# ws evicted; that mark is durable — a median sample pool below two workers
# freezes judgement and never clears it — so this phase only settles the idle
# worker. Phase 6 accepts either an existing mark or a new one.
DEADLINE=$((SECONDS + 120))
until worker_idle "$(worker_stats)" idle-worker 5; do
	(( SECONDS < DEADLINE )) || fail "idle worker never completed 5 warmup jobs"
	sleep 1
done
IDLE_COMPLETED="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["workers"]["idle-worker"]["completed"])' "$(worker_stats)")"
(( IDLE_COMPLETED >= 5 )) || fail "idle worker did not pass the warmup gate"

DEADLINE=$((SECONDS + 60))
until python3 -c '
import json, sys
s = json.loads(sys.argv[1])
w = s["workers"]["idle-worker"]
ok = w["ewma"] < 0.02 and w["jobs_per_sec"] == 0 and w["active"] == 0
sys.exit(0 if ok else 1)
' "$(worker_stats)"; do
	(( SECONDS < DEADLINE )) || fail "idle worker never settled into the idle range"
	sleep 1
done

# Phase 6: keep the queue non-empty so wf and ws keep finishing short jobs,
# then wait for the ratio judgement to hold with a non-zero median. ws may
# already carry a mark from the warmup load — marks are durable — so the loop
# accepts an existing mark once the idle worker has been measured against a
# non-zero median. The idle worker's next pull is far beyond this window, so
# its completed count cannot grow; past the warmup gate the only protection
# from the ratio judgement is the idle sample exclusion, and the EWMA
# assertion proves the worker was inside judging range. A median built only
# from decayed tails (no pool member reported a completion) is not a
# measurement and is skipped until wf reports a real throughput sample.
EVICTED=0
WS_COMPLETED=0
MEASURED=0
DEADLINE=$((SECONDS + 60))
i=0
while :; do
	(( SECONDS < DEADLINE )) || fail "slow worker was never evicted"
	i=$((i + 1))
	submit_job "flow-$i" "$FILE_ID" >/dev/null
	STATS="$(worker_stats)"
	RC=0
	python3 -c '
import json, sys
s = json.loads(sys.argv[1])
expected = int(sys.argv[2])
workers = s["workers"]
assert workers.get("wf", {}).get("evicted") is False, "fast worker wrongly evicted: %r" % workers
idle = workers.get("idle-worker")
assert idle is not None, "idle worker missing from the worker panel"
assert idle["completed"] >= 5, "idle worker never passed the warmup gate: %r" % idle
assert idle["completed"] == expected, "idle worker was given jobs: %r" % idle
assert idle["jobs_per_sec"] == 0 and idle["active"] == 0, "idle worker is not idle: %r" % idle
assert idle["evicted"] is False, "idle worker wrongly evicted: %r" % idle
if s["median"] <= 0 or workers.get("wf", {}).get("jobs_per_sec", 0) <= 0:
    sys.exit(3)
assert idle["ewma"] < s["median"] / 3, (
    "idle worker EWMA %r not below median/3 %r: idle exclusion not exercised"
    % (idle["ewma"], s["median"] / 3))
' "$STATS" "$IDLE_COMPLETED" || RC=$?
	case "$RC" in
	0) MEASURED=1 ;;
	3) ;;
	*) fail "idle/fast worker misjudged" ;;
	esac
	if [[ "$MEASURED" == "1" ]] &&
		python3 -c 'import json,sys; sys.exit(0 if json.loads(sys.argv[1])["workers"].get("ws",{}).get("evicted") else 1)' "$STATS"; then
		EVICTED=1
		WS_COMPLETED="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["workers"]["ws"]["completed"])' "$STATS")"
		break
	fi
	sleep 0.2
done
[[ "$EVICTED" == "1" ]] || fail "slow worker was never evicted"
(( WS_COMPLETED >= 5 )) || fail "evicted worker had $WS_COMPLETED completed jobs; warmup gate violated"
[[ "$MEASURED" == "1" ]] || fail "idle worker was never compared against a non-zero cluster median"

# require_ws_evicted fails unless the panel still reports ws as evicted. Every
# no-allocation observation below is gated on it, so a cleared mark can never
# turn the assertion into a vacuous pass.
require_ws_evicted() {
	local stats
	stats="$(worker_stats)" || fail "worker panel unavailable $1"
	python3 -c '
import json, sys
w = json.loads(sys.argv[1])["workers"].get("ws") or {}
sys.exit(0 if w.get("evicted") is True else 1)
' "$stats" || fail "ws eviction mark was cleared $1"
}

# Phase 7: while ws carries the eviction mark, submitted jobs must not land on
# it. The mark is only cleared by throughput recovery, so a median sample pool
# too small to judge freezes it. Every observation is taken with the mark
# asserted true: a clear fails the run instead of voiding the assertion.
[[ "$(job_field "$LONG_JOB" status)" == "running" ]] ||
	fail "parked job finished before the post-eviction phase; extend it"

# Collapse the median sample pool: stop submitting and wait for wf to leave it
# (two consecutive zero-throughput heartbeats) while ws holds its parked job.
# The panel then reports a zero median — too few samples to judge — and the
# mark must survive. Without this window the assertion below would not cover
# the pool-collapse path that used to clear the mark.
DEADLINE=$((SECONDS + 60))
until python3 -c '
import json, sys
s = json.loads(sys.argv[1])
ws = s["workers"].get("ws") or {}
wf = s["workers"].get("wf") or {}
ok = (ws.get("evicted") is True
      and s.get("median") == 0
      and wf.get("active") == 0
      and wf.get("jobs_per_sec") == 0)
sys.exit(0 if ok else 1)
' "$(worker_stats)"; do
	(( SECONDS < DEADLINE )) || fail "median sample pool never collapsed below two samples with ws evicted"
	sleep 0.5
done
require_ws_evicted "while the median sample pool was too small to judge"

POST_JOBS=()
for i in 1 2 3; do
	require_ws_evicted "before submitting post-$i"
	POST_JOBS+=("$(submit_job "post-$i" "$FILE_ID")")
done
for JOB in "${POST_JOBS[@]}"; do
	DEADLINE=$((SECONDS + 90))
	W=""
	until [[ -n "$W" && "$W" != "None" ]]; do
		(( SECONDS < DEADLINE )) || fail "post-eviction job $JOB was never assigned"
		W="$(job_field "$JOB" worker_name 2>/dev/null || true)"
		require_ws_evicted "while post-eviction job $JOB awaited assignment"
		[[ -n "$W" && "$W" != "None" ]] && break
		sleep 0.5
	done
	[[ "$W" != "ws" ]] || fail "new job $JOB assigned to the evicted worker"
	require_ws_evicted "after post-eviction job $JOB was assigned"
done

# Phase 8: eviction must not interrupt in-flight work.
DEADLINE=$((SECONDS + 240))
while :; do
	PENDING="$(api_get /api/v1/jobs | python3 -c '
import json, sys
jobs = json.load(sys.stdin)["jobs"]
print(sum(1 for j in jobs if j["status"] in ("pending", "queued", "running")))
')"
	(( PENDING == 0 )) && break
	(( SECONDS < DEADLINE )) || fail "$PENDING jobs still in flight after eviction"
	sleep 2
done
api_get /api/v1/jobs | python3 -c '
import json, sys
jobs = json.load(sys.stdin)["jobs"]
bad = [(j["id"], j["status"]) for j in jobs if j["status"] != "completed"]
assert not bad, "interrupted jobs after eviction: %r" % bad
' || fail "eviction interrupted existing jobs"

echo "NOTE: the recovery half (ewma >= median/1.5 clears the mark) is E2E-unreachable by design and covered by implementation-side tests"
echo "PASS: slow worker evicted after $WS_COMPLETED completed jobs, no new assignments, in-flight jobs completed"
