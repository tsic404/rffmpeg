#!/usr/bin/env bash
# TC-selfheal-03: 慢 Worker 驱逐（AC-45）
# 判据：比值口径命中后标记驱逐且不再分配新作业；样本池不足两个样本时不构成
# 判定、标记保持；已有作业跑到终态；空闲 Worker 不误判；warmup 门槛生效。
# 恢复半段 E2E 不可达（设计已注明），由实现侧断言覆盖。
#
# 编排要点：
# - RFFMPEG_THROUGHPUT_SCALE 把 ws 的上报吞吐压到 5%：完成量照常上报（不触发
#   busy-exempt），EWMA 却必然低于中位数，比值口径因此可稳定命中。
# - 调度器只把作业派给「空闲」状态的 Worker；ws 被长作业占住，因此它的作业只能
#   经拉取路径获得，且拉到的作业立即开跑，不会停在 queued 上被驱逐卡死。
# - ws 的心跳间隔长于健康检查间隔：一次完成在下一个心跳后仍可见，探测周期不会
#   与巡检相位锁定而永远错过判定窗口。
# - 空闲 Worker 只声明 mpeg2video 并拉长拉取周期：它的 warmup 作业不会落到别的
#   Worker，观察窗口内它既过门槛又真正空闲，唯一让它免于判定的就是空闲样本排除。
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

# Phase 1: warm ws past the >=5 gate; it is the only libx264 worker, so every
# submission here is deterministic.
WS_CONFIG="$TMP_ROOT/ws-worker.json"
cat >"$WS_CONFIG" <<'JSON'
{
  "auto_detect_codecs": false,
  "manual_encoders": ["libx264"],
  "manual_decoders": ["h264"]
}
JSON
start_worker ws RFFMPEG_CONFIG="$WS_CONFIG" \
	RFFMPEG_HEARTBEAT_INTERVAL=3s RFFMPEG_POLL_INTERVAL=300ms RFFMPEG_MAX_CONCURRENT=2
wait_worker ws

SHORT="$(make_media sel-short.mp4 1 320x240)"
BLOCK="$(make_media sel-block.mp4 20 640x480)"
FILLER="$(make_media sel-filler.mp4 180 640x480)"
LONG="$(make_media sel-long.mp4 180 1280x720)"
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$SHORT" -c:v libx264 "$TMP_ROOT/warm0.mp4" >/dev/null 2>"$TMP_ROOT/warm0.log"
FILE_ID="$(grep -oE 'Uploaded: .* -> [0-9a-f]+' "$TMP_ROOT/warm0.log" | awk '{print $NF}' || true)"
[[ -n "$FILE_ID" ]] || fail "short input file id not captured"

"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	probe "$LONG" >/dev/null 2>"$TMP_ROOT/long.log"
FILE_ID_LONG="$(grep -oE 'Uploaded: .* -> [0-9a-f]+' "$TMP_ROOT/long.log" | awk '{print $NF}' || true)"
[[ -n "$FILE_ID_LONG" ]] || fail "long input file id not captured"

"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	probe "$BLOCK" >/dev/null 2>"$TMP_ROOT/block.log"
FILE_ID_BLOCK="$(grep -oE 'Uploaded: .* -> [0-9a-f]+' "$TMP_ROOT/block.log" | awk '{print $NF}' || true)"
[[ -n "$FILE_ID_BLOCK" ]] || fail "probe input file id not captured"

"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	probe "$FILLER" >/dev/null 2>"$TMP_ROOT/filler.log"
FILE_ID_FILLER="$(grep -oE 'Uploaded: .* -> [0-9a-f]+' "$TMP_ROOT/filler.log" | awk '{print $NF}' || true)"
[[ -n "$FILE_ID_FILLER" ]] || fail "hold input file id not captured"

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

# Phase 2: park the long job on ws and hold its second slot until the fast
# worker is warm. The parked job verifies that eviction does not interrupt
# in-flight work; the hold keeps a pending job submitted for another worker
# from being claimed by ws's pull, which does not filter by encoder.
LONG_JOB="$(submit_job sel-long "$FILE_ID_LONG" veryslow 1)"
wait_job_status "$LONG_JOB" 30 "running" >/dev/null || fail "long job never ran"
[[ "$(job_field "$LONG_JOB" worker_name)" == "ws" ]] || fail "long job did not land on ws"
HOLD_JOB="$(submit_job sel-hold "$FILE_ID_FILLER" veryslow 1)"
wait_job_status "$HOLD_JOB" 30 "running" >/dev/null || fail "slot hold job never ran"
[[ "$(job_field "$HOLD_JOB" worker_name)" == "ws" ]] || fail "slot hold job did not land on ws"

# Phase 3: register the idle worker with a long pull interval. Only it declares
# mpeg2video, so the six warmup jobs cannot be scheduled elsewhere; they are
# delivered together at its first pull, after which its next pull is far beyond
# the observation window.
IDLE_CONFIG="$TMP_ROOT/idle-worker.json"
cat >"$IDLE_CONFIG" <<'JSON'
{
  "auto_detect_codecs": false,
  "manual_encoders": ["mpeg2video"],
  "manual_decoders": ["h264"]
}
JSON
start_worker idle-worker RFFMPEG_CONFIG="$IDLE_CONFIG" \
	RFFMPEG_HEARTBEAT_INTERVAL=3s RFFMPEG_POLL_INTERVAL="$IDLE_POLL_INTERVAL" \
	RFFMPEG_MAX_CONCURRENT=6
wait_worker idle-worker
for i in $(seq 1 6); do submit_job "idle-warm-$i" "$FILE_ID" medium 1 mpeg2video >/dev/null; done

# Phase 4: bring the fast worker in and warm it past the >=5 gate. It is the
# only idle-status libx264 worker, so the dispatcher hands it every submission
# while ws sits busy behind its parked job.
WF_CONFIG="$TMP_ROOT/wf-worker.json"
cat >"$WF_CONFIG" <<'JSON'
{
  "auto_detect_codecs": false,
  "manual_encoders": ["libx264"],
  "manual_decoders": ["h264"]
}
JSON
# Same heartbeat cadence as ws: the EWMA decay rate decides how fast a mark can
# look "recovered" after the last sample, and a slower-decaying evicted worker
# would otherwise overtake its peer's decayed tail.
start_worker wf RFFMPEG_CONFIG="$WF_CONFIG" \
	RFFMPEG_HEARTBEAT_INTERVAL=3s RFFMPEG_POLL_INTERVAL=300ms RFFMPEG_MAX_CONCURRENT=1
wait_worker wf
wf_completed() {
	worker_stats | python3 -c '
import json, sys
print(json.loads(sys.stdin.read())["workers"].get("wf", {}).get("completed", 0))
'
}
DEADLINE=$((SECONDS + 180))
for i in $(seq 1 5); do
	BEFORE="$(wf_completed)"
	submit_job "wf-warm-$i" "$FILE_ID" ultrafast 1 libx264 >/dev/null
	until [[ "$(wf_completed)" -gt "$BEFORE" ]]; do
		(( SECONDS < DEADLINE )) || fail "fast worker never completed warmup job $i"
		sleep 0.2
	done
done

# Phase 5: wait for the idle worker's first pull to deliver its six queued
# jobs, then let the warmup EWMA decay. The warmup load can already have marked
# ws evicted; that mark is durable — a median sample pool below two workers
# freezes judgement and never clears it — so this phase only settles the idle
# worker. Phase 6 accepts either an existing mark or a new one. The budget
# covers the worker's first pull interval plus the EWMA settling time, with
# headroom for a loaded host.
DEADLINE=$((SECONDS + 180))
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

# Phase 6: hand both workers one short probe per round. The dispatcher gives
# the first to wf; the second stays pending because wf runs at capacity, so ws
# claims it on its next pull. Both finish together, so the judgement that reads
# ws's fresh sample also finds wf producing inside the median pool.
MEASURED=0
EVICTED=0
WS_COMPLETED=0
PROBES=""
PROBE_DONE_AT=-10
PROBE_N=0
DEADLINE=$((SECONDS + 180))
while :; do
	(( SECONDS < DEADLINE )) || fail "slow worker was never evicted"
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

	if python3 -c '
import json, sys
ws = (json.loads(sys.argv[1])["workers"].get("ws") or {})
sys.exit(0 if ws.get("evicted") is True else 1)
' "$STATS"; then
		EVICTED=1
		WS_COMPLETED="$(python3 -c 'import json,sys; print((json.loads(sys.argv[1])["workers"].get("ws") or {}).get("completed",0))' "$STATS")"
	fi
	# The idle-exclusion assertion needs a live median sample, which only wf
	# supplies; keep probing until it has held at least once.
	[[ "$EVICTED" == "1" && "$MEASURED" == "1" ]] && break

	REMAIN=""
	for JOB in $PROBES; do
		STATUS="$(job_field "$JOB" status 2>/dev/null || true)"
		case "$STATUS" in
		completed|failed|cancelled)
			if [[ "$(job_field "$JOB" worker_name 2>/dev/null || true)" == "ws" ]]; then
				PROBE_DONE_AT=$SECONDS
			fi
			;;
		*) REMAIN="$REMAIN $JOB" ;;
		esac
	done
	PROBES="${REMAIN# }"

	if [[ -z "$PROBES" ]] && (( SECONDS - PROBE_DONE_AT >= 3 )) &&
		python3 -c '
import json, sys
ws = (json.loads(sys.argv[1])["workers"].get("ws") or {})
sys.exit(0 if ws.get("jobs_per_sec", 0) == 0 else 1)
' "$STATS"; then
		for _ in 1 2; do
			PROBE_N=$((PROBE_N + 1))
			PROBES="$PROBES $(submit_job "flow-probe-$PROBE_N" "$FILE_ID_BLOCK" veryslow 1 libx264)"
		done
		PROBES="${PROBES# }"
	fi
	sleep 0.2
done
(( WS_COMPLETED >= 5 )) || fail "evicted worker had $WS_COMPLETED completed jobs; warmup gate violated"
[[ "$MEASURED" == "1" ]] || fail "idle worker was never compared against a non-zero cluster median"

# Phase 7: require_ws_evicted fails unless the panel still reports ws as
# evicted. Every no-allocation observation below is gated on it, so a cleared
# mark can never turn the assertion into a vacuous pass.
require_ws_evicted() {
	local stats
	stats="$(worker_stats)" || fail "worker panel unavailable $1"
	python3 -c '
import json, sys
w = json.loads(sys.argv[1])["workers"].get("ws") or {}
sys.exit(0 if w.get("evicted") is True else 1)
' "$stats" || fail "ws eviction mark was cleared $1"
}

[[ "$(job_field "$LONG_JOB" status)" == "running" ]] ||
	fail "parked job finished before the post-eviction phase; extend it"

# Drain the probes still in flight and keep the healthy peer's throughput
# sample fresh while doing it: the evicted worker's EWMA only has to clear
# median/RecoveryThreshold to be un-marked, and a peer sample that decayed while
# the peer sat idle would let exactly that happen before the pool collapses. The
# evicted worker cannot take these jobs (its pulls are refused), so they can
# only run on the peer.
DEADLINE=$((SECONDS + 120))
FEED=""
FEED_N=0
FEED_DONE=0
while :; do
	PROBES_PENDING=0
	for JOB in $PROBES; do
		case "$(job_field "$JOB" status 2>/dev/null || true)" in
		completed|failed|cancelled) ;;
		*) PROBES_PENDING=1 ;;
		esac
	done
	if [[ -n "$FEED" ]]; then
		case "$(job_field "$FEED" status 2>/dev/null || true)" in
		completed|failed|cancelled) FEED=""; FEED_DONE=1 ;;
		esac
	fi
	[[ "$PROBES_PENDING" == "0" && "$FEED_DONE" == "1" ]] && break
	(( SECONDS < DEADLINE )) || fail "probes or the peer refresh job never finished"
	if [[ -z "$FEED" ]]; then
		FEED_N=$((FEED_N + 1))
		FEED="$(submit_job "phase7-feed-$FEED_N" "$FILE_ID" ultrafast 1 libx264)"
		FEED_DONE=0
	fi
	require_ws_evicted "while the peer kept a fresh throughput sample"
	sleep 0.5
done

# COLLAPSE_HOLD_S is three health-check intervals: two for wf to leave the
# median sample pool after its last completion, plus one as margin for a
# completion that has not reached the state table yet. A late sample still
# resets the window through the frozen completed counter.
COLLAPSE_HOLD_S=6

# collapse_window LABEL BUDGET
# Blocks until the median sample pool has stayed below two samples — median 0,
# no job on wf, and wf's completed counter frozen — with ws still marked, for
# COLLAPSE_HOLD_S. A completion restarts the clock, so the wait never assumes
# the pool stays collapsed on its own; a cleared mark fails at once through
# require_ws_evicted, which is why an implementation that clears the mark on a
# small sample pool cannot get through this window.
collapse_window() {
	local label="$1" deadline=$((SECONDS + $2)) start="" frozen=""
	local stats collapsed evicted completed
	while :; do
		require_ws_evicted "while establishing the $label"
		stats="$(worker_stats)"
		read -r collapsed evicted completed <<<"$(
			python3 -c '
import json, sys
s = json.loads(sys.argv[1])
ws = s["workers"].get("ws") or {}
wf = s["workers"].get("wf") or {}
print(1 if s.get("median") == 0 and wf.get("active", 0) == 0 else 0,
      "True" if ws.get("evicted") is True else "False",
      wf.get("completed", 0))
' "$stats"
		)"
		if [[ "$collapsed" == "1" && "$evicted" == "True" ]]; then
			if [[ -z "$start" || "$completed" != "$frozen" ]]; then
				start=$SECONDS
				frozen=$completed
			elif (( SECONDS - start >= COLLAPSE_HOLD_S )); then
				return 0
			fi
		else
			start=""
		fi
		(( SECONDS < deadline )) ||
			fail "$label never held below two samples for ${COLLAPSE_HOLD_S}s with ws evicted; panel=$stats"
		sleep 0.5
	done
}

collapse_window "pool collapse before the post-eviction jobs" 120
require_ws_evicted "while the median sample pool was too small to judge"

POST_JOBS=()
for i in 1 2 3; do
	require_ws_evicted "before submitting post-$i"
	POST_JOBS+=("$(submit_job "post-$i" "$FILE_ID" ultrafast 1 libx264)")
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

# The post-eviction jobs fed wf again, so the pool has to collapse a second
# time before the run can end: the mark must survive that second window too,
# observed for the same hold period rather than inferred from the first one.
collapse_window "pool collapse after the post-eviction jobs" 120

# Phase 8: eviction must not interrupt in-flight work. The parked job outlives
# Phases 6-7, so this phase waits for it plus the short jobs.
DEADLINE=$((SECONDS + 300))
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
bad = [(j.get("output_filename"), j["status"], j.get("failure_type")) for j in jobs if j["status"] != "completed"]
assert not bad, "interrupted jobs after eviction: %r" % bad
' || fail "eviction interrupted existing jobs"

echo "NOTE: the recovery half (ewma >= median/1.5 clears the mark) is E2E-unreachable by design and covered by implementation-side tests"
echo "PASS: slow worker evicted after $WS_COMPLETED completed jobs, no new assignments, in-flight jobs completed"
