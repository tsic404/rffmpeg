#!/usr/bin/env bash
# TC-selfheal-05: 被驱逐 Worker 的人工清除端到端（AC-47）
# 判据：慢 Worker 被驱逐且跨 Server 重启标记保持（期间无自动恢复）→ 人工清除
# → 面板标记消失、该 Worker 在下一个心跳周期回到可调度池 → 新提交的作业落在
# 其上并完成（rc=0、输出可 ffprobe）→ 累计完成数不重置；吞吐注入仍生效时，
# 下一次有效判定把它重新驱逐。
#
# 编排要点：
# - 吞吐注入把 slow-node 的上报吞吐压到 2%，两个 worker 都过 warmup 门槛后，
#   比值口径必然命中；清除后的重新驱逐仍由同一开关驱动。
# - 驱逐后先排空队列再重启，避免重启把在跑作业重置回 pending、干扰后续落点判定。
# - 驱逐标记的保持判据是「窗口外」（C-118）：排空后必须等 slow-node 连续两次
#   零吞吐心跳、退出中位数样本池，再重启。仍在池内时，作业结束后的完成样本可能
#   与 fast-node 衰减后的 EWMA 构成一次合法的窗口内恢复，标记会在重启前消失，
#   让重启断言变成假失败；样本池不足两个样本后不再有恢复判定。
# - 重启后 evicted worker 的心跳与拉取都被拒（C-79），它不会重新注册；另一个
#   worker 会自己回到池子。此时提交一条长作业：慢节点还处于驱逐，唯一可调度的
#   fast-node 必然接住它，从而在「清除后新作业落点」阶段占住自己的唯一槽位。
# - 清除后 slow-node 的第一次心跳会收到 404（离线）并触发重新注册，这是它回到
#   可调度池的路径；面板 status 从 offline 变回 idle 即该事件的可观察证据。
# - 清除后的新作业用 mpeg2video：只有 slow-node 声明该编码器，且 fast-node 的
#   槽位被长作业占住（拉取路径按容量拦截），落点因此可判定。
# - 审计不可经端点读取（C-91）：这里只直读 SQLite 断言「人工清除写了一条恢复
#   事件」与「没有额外的自动恢复事件」；两种 reason 的差异由实现侧测试断言。
set -euo pipefail

PORT="${PORT:-18999}"
export RFFMPEG_THROUGHPUT_SCALE="slow-node=0.02"
SERVER_EXTRA_FLAGS="--worker-health-check-interval 2s --worker-heartbeat-timeout 20s"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

CLEAR_PATH="/admin/workers/eviction/clear"
DB_PATH="$DATA_DIR/rffmpeg.db"

build_binaries
start_server
wait_server_ready

PROBE_MEDIA="$(make_media sh5-probe.mp4 12 640x480)"
LONG_MEDIA="$(make_media sh5-long.mp4 180 1280x720)"

SLOW_CONFIG="$TMP_ROOT/slow.json"
cat >"$SLOW_CONFIG" <<'JSON'
{
  "auto_detect_codecs": false,
  "manual_encoders": ["libx264", "mpeg2video"],
  "manual_decoders": ["h264"]
}
JSON
FAST_CONFIG="$TMP_ROOT/fast.json"
cat >"$FAST_CONFIG" <<'JSON'
{
  "auto_detect_codecs": false,
  "manual_encoders": ["libx264"],
  "manual_decoders": ["h264"]
}
JSON
start_worker slow-node RFFMPEG_CONFIG="$SLOW_CONFIG" \
	RFFMPEG_HEARTBEAT_INTERVAL=2s RFFMPEG_POLL_INTERVAL=300ms RFFMPEG_MAX_CONCURRENT=1
start_worker fast-node RFFMPEG_CONFIG="$FAST_CONFIG" \
	RFFMPEG_HEARTBEAT_INTERVAL=2s RFFMPEG_POLL_INTERVAL=300ms RFFMPEG_MAX_CONCURRENT=1
wait_worker slow-node
wait_worker fast-node

# The CLI probe uploads the input and dispatches the ffprobe work to a worker,
# so it needs a registered worker before it can run.
upload_file_id() {
	local media="$1" log="$2"
	"$RFFMPEG_BIN/rffmpeg" \
		probe "$media" >/dev/null 2>"$log"
	grep -oE 'Uploaded: .* -> [0-9a-f]+' "$log" | awk '{print $NF}' || true
}
FILE_ID="$(upload_file_id "$PROBE_MEDIA" "$TMP_ROOT/probe.log")"
FILE_ID_LONG="$(upload_file_id "$LONG_MEDIA" "$TMP_ROOT/long.log")"
[[ -n "$FILE_ID" && -n "$FILE_ID_LONG" ]] || fail "probe input file ids not captured"

worker_stats() {
	api_get /api/v1/workers |
		python3 -c '
import json, sys
data = json.load(sys.stdin)
out = {"median": data.get("cluster_median_throughput", 0), "workers": {}}
for w in data["workers"]:
    health = w.get("health") or {}
    out["workers"][w["name"]] = {
        "id": w["id"],
        "evicted": w.get("evicted"),
        "status": w.get("status"),
        "last_heartbeat": w.get("last_heartbeat"),
        "completed": health.get("completed_jobs", 0),
        "jobs_per_sec": health.get("jobs_per_sec", 0),
        "ewma": health.get("ewma_jobs_per_sec", 0),
        "active": len(health.get("active_jobs") or []),
    }
print(json.dumps(out))
'
}

panel_field() {
	python3 -c '
import json, sys
w = (json.loads(sys.argv[1])["workers"] or {}).get(sys.argv[2]) or {}
print(w.get(sys.argv[3], ""))
' "$1" "$2" "$3"
}

# submit_job_http NAME FILE_ID PRESET THREADS prints the new job id.
submit_job_http() {
	local name="$1" file_id="$2" preset="$3" threads="$4" attempt resp code
	local args="[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\",\"-preset\",\"$preset\",\"-threads\",\"$threads\",\"-y\"]"
	for attempt in $(seq 1 60); do
		resp="$(curl -sS -X POST -H "Authorization: Bearer $AUTH_TOKEN" \
			-H 'Content-Type: application/json' \
			-d "{\"input_files\":[\"$file_id\"],\"args\":$args,\"output_filename\":\"$name.mp4\"}" \
			-w '\n%{http_code}' "$(acceptance_server_url)/api/v1/jobs")"
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

inflight_jobs() {
	api_get /api/v1/jobs | python3 -c '
import json, sys
jobs = json.load(sys.stdin)["jobs"]
print(sum(1 for j in jobs if j["status"] in ("pending", "queued", "running")))
'
}

audit_rows() {
	local sql="$1"
	shift
	python3 - "$DB_PATH" "$sql" "$@" <<'PY'
import sqlite3, sys
path, sql, args = sys.argv[1], sys.argv[2], sys.argv[3:]
con = sqlite3.connect("file:%s?mode=ro" % path, uri=True, timeout=5)
try:
    for row in con.execute(sql, args):
        print("\t".join("" if v is None else str(v) for v in row))
finally:
    con.close()
PY
}

recovered_rows() {
	audit_rows "SELECT COUNT(*) FROM worker_eviction_events WHERE worker_id = ? AND event_type = 'recovered'" "$1"
}

recovery_audit_seq() {
	audit_rows "SELECT COALESCE(MAX(rowid), 0) FROM worker_eviction_events WHERE worker_id = ? AND event_type = 'recovered'" "$1"
}

drain_inflight() {
	local budget="$1"
	local deadline=$((SECONDS + budget))
	until [[ "$(inflight_jobs)" == "0" ]]; do
		(( SECONDS < deadline )) || return 1
		sleep 1
	done
}

dump_restart_state() {
	local label="$1" stats="$2" slow_id="${SLOW_ID:-}"
	if [[ -z "$slow_id" ]]; then
		slow_id="$(audit_rows "SELECT id FROM workers WHERE name = 'slow-node' ORDER BY created_at DESC LIMIT 1" 2>/dev/null || true)"
	fi
	{
		echo "=== $label ==="
		echo "--- panel ---"
		printf '%s\n' "$stats"
		echo "--- sample pool ---"
		python3 -c '
import json, sys
data = json.loads(sys.argv[1] or "{}")
print("cluster_median=%s" % data.get("median", ""))
for name, w in sorted((data.get("workers") or {}).items()):
    print("sample %s: jobs_per_sec=%s ewma=%s active=%s evicted=%s" % (
        name, w.get("jobs_per_sec"), w.get("ewma"), w.get("active"), w.get("evicted")))
' "$stats" 2>/dev/null || true
		echo "--- workers ---"
		audit_rows "SELECT id, name, status, evicted, evicted_at, last_heartbeat FROM workers ORDER BY created_at" || true
		echo "--- recovery audit (slow-node) ---"
		if [[ -n "$slow_id" ]]; then
			printf 'recovered_rows=%s recovery_audit_seq=%s\n' \
				"$(recovered_rows "$slow_id" 2>/dev/null || true)" \
				"$(recovery_audit_seq "$slow_id" 2>/dev/null || true)"
		else
			echo "slow-node row not present; recovery audit sequence unavailable"
		fi
		echo "--- eviction events (slow-node, recent 20) ---"
		if [[ -n "$slow_id" ]]; then
			audit_rows "SELECT e.rowid, e.timestamp, e.event_type, w.name, e.current_throughput, e.cluster_median, e.decision_reason FROM worker_eviction_events e LEFT JOIN workers w ON w.id = e.worker_id WHERE e.worker_id = ? ORDER BY e.rowid DESC LIMIT 20" "$slow_id" || true
		else
			audit_rows "SELECT e.rowid, e.timestamp, e.event_type, w.name, e.current_throughput, e.cluster_median, e.decision_reason FROM worker_eviction_events e LEFT JOIN workers w ON w.id = e.worker_id ORDER BY e.rowid DESC LIMIT 20" || true
		fi
		echo "--- server log tail ---"
		tail -30 "$SERVER_LOG" || true
	} >&2
}

pump_until_evicted() {
	local target="$1" budget="$2"
	local deadline=$((SECONDS + budget)) stats n=0
	while :; do
		stats="$(worker_stats)"
		if python3 -c '
import json, sys
workers = json.loads(sys.argv[1])["workers"]
target = workers.get(sys.argv[2]) or {}
peer = workers.get(sys.argv[3]) or {}
sys.exit(0 if target.get("evicted") is True and peer.get("evicted") is False else 1)
' "$stats" "$target" "fast-node"; then
			return 0
		fi
		(( SECONDS < deadline )) || return 1
		if (( $(inflight_jobs) < 6 )); then
			n=$((n + 1))
			submit_job_http "sh5-pump-$n" "$FILE_ID" veryslow 1 >/dev/null
			n=$((n + 1))
			submit_job_http "sh5-pump-$n" "$FILE_ID" veryslow 1 >/dev/null
		fi
		sleep 0.3
	done
}

# wait_slow_out_of_sample_pool blocks until slow-node has reported two
# consecutive zero-throughput heartbeats with no active job and no completion
# count change. At that point it is out of the median sample pool (C-82), so the
# health monitor cannot run a recovery judgement on it (C-118). A legitimate
# in-window recovery is handled by re-pumping only when a recovery audit row
# appears after the last observation of the mark. The heartbeat boundary is
# updated before that decision, so the second zero sample makes any loss
# out-of-window; historical, unaudited and out-of-pool losses all fail.
wait_slow_out_of_sample_pool() {
	local budget="$1"
	local deadline=$((SECONDS + budget))
	local stats="" hb="" jps="" active="" completed="" baseline_completed=""
	local zero_hbs=0 last_hb=""
	local baseline_recovery_seq="" seq_before="" seq_after=""
	baseline_recovery_seq="$(recovery_audit_seq "$SLOW_ID" 2>/dev/null || true)"
	if [[ ! "$baseline_recovery_seq" =~ ^[0-9]+$ ]]; then
		echo "FAIL: the recovery audit sequence was not readable before the sample-pool wait" >&2
		return 1
	fi
	while :; do
		seq_before="$(recovery_audit_seq "$SLOW_ID" 2>/dev/null || true)"
		if [[ ! "$seq_before" =~ ^[0-9]+$ ]]; then
			echo "FAIL: the recovery audit sequence was not readable while waiting for slow-node to leave the median sample pool" >&2
			return 1
		fi
		stats="$(worker_stats 2>/dev/null || true)"
		if [[ -z "$stats" ]]; then
			(( SECONDS < deadline )) || {
				echo "FAIL: the workers panel did not answer while waiting for slow-node to leave the median sample pool" >&2
				return 1
			}
			sleep 0.5
			continue
		fi
		if [[ -z "$(panel_field "$stats" slow-node id)" ]]; then
			dump_restart_state "the slow-node row disappeared while waiting for it to leave the median sample pool" "$stats"
			echo "FAIL: the slow-node row disappeared while waiting for it to leave the median sample pool" >&2
			return 1
		fi
		hb="$(panel_field "$stats" slow-node last_heartbeat)"
		jps="$(panel_field "$stats" slow-node jobs_per_sec)"
		active="$(panel_field "$stats" slow-node active)"
		completed="$(panel_field "$stats" slow-node completed)"
		if [[ "$active" == "0" ]] &&
			python3 -c 'import sys; sys.exit(0 if float(sys.argv[1]) == 0 else 1)' "$jps"; then
			if [[ -z "$baseline_completed" || "$completed" != "$baseline_completed" ]]; then
				# A late completion may still produce one non-zero throughput
				# sample on its next heartbeat; restart the observation.
				baseline_completed="$completed"
				last_hb="$hb"
				zero_hbs=1
			elif [[ "$hb" != "$last_hb" ]]; then
				last_hb="$hb"
				zero_hbs=$((zero_hbs + 1))
			fi
		else
			baseline_completed="" last_hb="" zero_hbs=0
		fi
		if [[ "$(panel_field "$stats" slow-node evicted)" != "True" ]]; then
			seq_after="$(recovery_audit_seq "$SLOW_ID" 2>/dev/null || true)"
			if [[ ! "$seq_after" =~ ^[0-9]+$ ]]; then
				echo "FAIL: the recovery audit sequence was not readable after the eviction mark disappeared" >&2
				return 1
			fi
			if (( zero_hbs < 2 )) && (( seq_after > baseline_recovery_seq )); then
				echo "slow-node recovered in-window with a new audit row (seq ${baseline_recovery_seq}->${seq_after}); re-pumping to re-establish the mark" >&2
				pump_until_evicted slow-node 240 || return 1
				drain_inflight 180 || return 1
				deadline=$((SECONDS + budget))
				baseline_completed="" last_hb="" zero_hbs=0
				baseline_recovery_seq="$seq_after"
				continue
			fi
			dump_restart_state "the eviction mark disappeared before slow-node left the median sample pool" "$stats"
			if (( zero_hbs >= 2 )); then
				echo "FAIL: the eviction mark disappeared after slow-node left the median sample pool" >&2
			else
				echo "FAIL: the eviction mark disappeared without a new recovery audit (seq ${baseline_recovery_seq}->${seq_after})" >&2
			fi
			return 1
		fi
		baseline_recovery_seq="$seq_before"
		if (( zero_hbs >= 2 )); then
			# One health tick after the second zero sample proves the pool
			# has fewer than two samples; no recovery judgement can run.
			sleep 2.5
			stats="$(worker_stats 2>/dev/null || true)"
			if [[ -z "$stats" ]]; then
				echo "FAIL: the workers panel did not answer after slow-node left the median sample pool" >&2
				return 1
			fi
			if [[ "$(panel_field "$stats" slow-node evicted)" != "True" ]]; then
				dump_restart_state "the eviction mark disappeared after slow-node left the median sample pool" "$stats"
				echo "FAIL: the eviction mark did not survive the health tick after slow-node left the median sample pool" >&2
				return 1
			fi
			echo "slow-node is out of the median sample pool (evicted=True, active=0, jobs_per_sec=0)" >&2
			return 0
		fi
		(( SECONDS < deadline )) || {
			dump_restart_state "slow-node did not leave the median sample pool within ${budget}s" "$stats"
			return 1
		}
		sleep 0.25
	done
}

# Phase 1: drive the slow worker out of the scheduling pool.
if ! pump_until_evicted slow-node 240; then
	dump_restart_state "slow-node was never evicted within the pump budget" "$(worker_stats 2>/dev/null || true)"
	fail "slow-node was never evicted"
fi
STATS="$(worker_stats)"
SLOW_ID="$(panel_field "$STATS" slow-node id)"
PRE_COMPLETED="$(panel_field "$STATS" slow-node completed)"
[[ -n "$SLOW_ID" ]] || fail "slow-node id not found in the panel"
(( PRE_COMPLETED >= 5 )) || fail "slow-node did not pass the warmup gate before eviction ($PRE_COMPLETED)"

# Phase 2: drain the pump queue before the restart so the restarted server has
# no inherited backlog; the hold job below is submitted only after the restart.
drain_inflight 180 || fail "pump jobs never drained before the restart"

# The eviction mark must be tested out-of-window (C-118): while slow-node is
# still in the median sample pool, a completion burst can legitimately recover
# it, and the restart assertion would misread that as a restart loss. Wait for
# two zero-throughput heartbeats with no active job first.
wait_slow_out_of_sample_pool 120 ||
	fail "could not establish the out-of-window eviction precondition"
STATS="$(worker_stats)"
[[ "$(panel_field "$STATS" slow-node evicted)" == "True" ]] ||
	fail "the eviction mark was not set immediately before the restart"
AUDIT_PRE="$(recovered_rows "$SLOW_ID")"

# Phase 3: the mark must survive a Server restart — and no automatic recovery may
# run while it does. The restarted server has an empty state table, so the evicted
# worker cannot produce a recovery sample; its row must still carry the mark. The
# worker has also left the pre-restart sample pool, so the old server cannot judge
# it during the restart window either.
stop_server
start_server
wait_server_ready

# Wait for the slow-node row to reappear in the panel. A missing row and a false
# mark are different failures; report which one happened.
deadline=$((SECONDS + 30))
until stats="$(worker_stats 2>/dev/null)" &&
	python3 -c '
import json, sys
sys.exit(0 if "slow-node" in json.loads(sys.argv[1])["workers"] else 1)
' "$stats"; do
	(( SECONDS < deadline )) || {
		dump_restart_state "the slow-node row disappeared after the restart" "$stats"
		fail "the slow-node row disappeared after the Server restart"
	}
	sleep 0.25
done

# The peer must come back online before it can hold a slot; the evicted worker
# must not (C-79 keeps it out until the manual clear).
deadline=$((SECONDS + 90))
until python3 -c '
import json, sys
w = (json.loads(sys.argv[1])["workers"] or {}).get("fast-node") or {}
sys.exit(0 if w.get("status") != "offline" else 1)
' "$(worker_stats)"; do
	(( SECONDS < deadline )) || fail "fast-node never re-registered after the restart"
done

# Observe continuously for at least three health-check ticks (2s each) plus one
# heartbeat period (2s). A single delayed read could miss a transient clear.
OBSERVE_START=$SECONDS
while (( SECONDS - OBSERVE_START < 12 )); do
	STATS="$(worker_stats 2>/dev/null || true)"
	[[ -n "$STATS" ]] || { sleep 0.5; continue; }
	if [[ "$(panel_field "$STATS" slow-node evicted)" != "True" ]]; then
		dump_restart_state "the eviction mark did not survive the restart observation window" "$STATS"
		fail "the eviction mark did not survive the Server restart"
	fi
	sleep 0.5
done
STATS="$(worker_stats 2>/dev/null || true)"
[[ -n "$STATS" && "$(panel_field "$STATS" slow-node evicted)" == "True" ]] ||
	{
		dump_restart_state "the eviction mark was not readable at the end of the restart window" "$STATS"
		fail "the eviction mark did not survive the Server restart"
	}
AUDIT_BEFORE="$(recovered_rows "$SLOW_ID")"
[[ "$AUDIT_BEFORE" == "$AUDIT_PRE" ]] ||
	fail "an automatic recovery was audited during the restart window ($AUDIT_PRE -> $AUDIT_BEFORE)"

# Phase 4: park a long job on the only schedulable worker (fast-node) so its
# single slot stays occupied through the post-clear placement check. The evicted
# worker is still offline and rejected, so the job can only land on fast-node.
HOLD_JOB="$(submit_job_http sh5-hold "$FILE_ID_LONG" veryslow 1)"
wait_job_status "$HOLD_JOB" 90 "running" >/dev/null || fail "hold job never ran"
[[ "$(job_field "$HOLD_JOB" worker_name)" == "fast-node" ]] ||
	fail "hold job landed on '$(job_field "$HOLD_JOB" worker_name)', want fast-node"

# Phase 5: manual clear by registered name.
HTTP="$(curl -sS -X POST -H "Authorization: Bearer $AUTH_TOKEN" \
	-H 'Content-Type: application/json' -d '{"worker":"slow-node"}' \
	-w '\n%{http_code}' "$(acceptance_server_url)$CLEAR_PATH")"
CLEAR_STATUS="${HTTP##*$'\n'}"
CLEAR_BODY="${HTTP%$'\n'*}"
[[ "$CLEAR_STATUS" == "200" ]] || fail "manual clear failed: HTTP $CLEAR_STATUS ($CLEAR_BODY)"
python3 -c '
import json, sys
body = json.loads(sys.argv[1])
assert body.get("cleared") is True, "cleared must be true: %r" % body
assert body.get("evicted") is False, "evicted must be false right after the clear: %r" % body
assert body.get("worker_id") == sys.argv[2], "wrong worker_id: %r" % body
assert body.get("worker_name") == "slow-node", "wrong worker_name: %r" % body
' "$CLEAR_BODY" "$SLOW_ID" || fail "unexpected clear response: $CLEAR_BODY"

AUDIT_AFTER="$(recovered_rows "$SLOW_ID")"
[[ "$AUDIT_AFTER" == "$((AUDIT_BEFORE + 1))" ]] ||
	fail "the manual clear did not write exactly one recovery audit event ($AUDIT_BEFORE -> $AUDIT_AFTER)"

# Phase 6: the mark is gone from the panel, and the worker re-enters the pool on
# its next heartbeat (it must re-register because the restart left it offline).
deadline=$((SECONDS + 90))
until python3 -c '
import json, sys
w = (json.loads(sys.argv[1])["workers"] or {}).get("slow-node") or {}
sys.exit(0 if w.get("evicted") is False and w.get("status") != "offline" else 1)
' "$(worker_stats)"; do
	(( SECONDS < deadline )) || fail "cleared worker never returned to the scheduling pool"
done

# Phase 7: a new job must land on the cleared worker and complete. Only
# slow-node declares mpeg2video, and fast-node's single slot is held by the long
# job, so neither dispatch nor the pull path can route it elsewhere.
python3 -c '
import json, sys
w = (json.loads(sys.argv[1])["workers"] or {}).get("fast-node") or {}
sys.exit(0 if w.get("active", 0) >= 1 else 1)
' "$(worker_stats)" || fail "the peer slot is no longer occupied; the placement check would be ambiguous"
OUTPUT="$TMP_ROOT/after-clear.mp4"
RC=0
"$RFFMPEG_BIN/rffmpeg" \
	-i "$PROBE_MEDIA" -c:v mpeg2video -q:v 4 -y "$OUTPUT" \
	>/dev/null 2>"$TMP_ROOT/after-clear.log" || RC=$?
expect_rc "$RC" 0 "post-clear transcode"
[[ -f "$OUTPUT" ]] || fail "post-clear transcode produced no output file"
DURATION="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUTPUT" 2>/dev/null || true)"
python3 -c '
import sys
duration = float(sys.argv[1])
sys.exit(0 if duration > 0 else 1)
' "${DURATION:-0}" || fail "post-clear output is not ffprobe-able (duration: ${DURATION:-})"

JOB_ID="$(job_id_from_log "$TMP_ROOT/after-clear.log")"
[[ -n "$JOB_ID" ]] || fail "post-clear job id not captured from the CLI log"
[[ "$(job_field "$JOB_ID" worker_name)" == "slow-node" ]] ||
	fail "post-clear job landed on '$(job_field "$JOB_ID" worker_name)', want slow-node"

# Cumulative completion count must not reset: the same worker process carries it
# across the re-registration forced by the restart.
deadline=$((SECONDS + 60))
until python3 -c '
import json, sys
w = (json.loads(sys.argv[1])["workers"] or {}).get("slow-node") or {}
sys.exit(0 if w.get("completed", 0) >= int(sys.argv[2]) else 1)
' "$(worker_stats)" "$PRE_COMPLETED"; do
	(( SECONDS < deadline )) || fail "completion count was reset by the clear (want >= $PRE_COMPLETED)"
done

# Phase 7: with the injection still active, the next valid judgement must mark the
# still-slow worker again — the clear released the mark, it did not rehabilitate
# the node. The hold job is cancelled first so both slots feed the judgement.
curl -sS -X DELETE -H "Authorization: Bearer $AUTH_TOKEN" \
	"$(acceptance_server_url)/api/v1/jobs/$HOLD_JOB" >/dev/null || true
if ! pump_until_evicted slow-node 240; then
	dump_restart_state "the cleared worker was not re-evicted within the pump budget" "$(worker_stats 2>/dev/null || true)"
	fail "cleared worker was not re-evicted despite the active injection"
fi
STATS="$(worker_stats)"
AFTER_COMPLETED="$(panel_field "$STATS" slow-node completed)"
(( AFTER_COMPLETED >= PRE_COMPLETED )) ||
	fail "completion count reset across the re-eviction ($AFTER_COMPLETED < $PRE_COMPLETED)"
[[ "$(recovered_rows "$SLOW_ID")" == "$AUDIT_AFTER" ]] ||
	fail "the automatic path wrote an extra recovery event for the manually cleared worker"

echo "PASS: eviction survived the restart, manual clear released it, the worker returned and completed a job (rc=0, ffprobe ok), and the still-slow node was re-evicted"
