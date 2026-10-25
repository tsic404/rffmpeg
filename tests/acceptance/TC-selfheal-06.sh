#!/usr/bin/env bash
# TC-selfheal-06: 人工清除接口的鉴权与错误语义（AC-48）
# 判据：无 token / 错 token → 401 且标记不变；未知对象 → 404 not_found；
# 未给出对象 → 400 invalid_request（两者都不改变任何标记）；未处于驱逐状态的
# 对象 → 成功且状态不变。
#
# 「不改变标记」以面板驱逐标记为准；「不写审计行」按 C-91 不经端点暴露，
# 这里直接只读 SQLite 的 worker_eviction_events 断言该实现侧事实。
#
# 编排要点：
# - 需要真实处于驱逐状态的对象，才谈得上「标记不变」。用与 TC-selfheal-03 相同的
#   吞吐注入 + 两个 worker 制造驱逐；注入只缩放上报吞吐，不拖慢 worker。
# - 每个巡检 tick 只读最后一次心跳样本来判定；单次 0 样本既可能是空闲也可能
#   是作业间隙，因此喂入一小队短作业，让判定读到有产出的区间。
# - 并发提交受默认每 client 10 个并发作业的限流约束，喂料保持少量在飞作业。
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

PROBE_MEDIA="$(make_media sh6-probe.mp4 12 640x480)"

WORKER_CONFIG="$TMP_ROOT/worker.json"
cat >"$WORKER_CONFIG" <<'JSON'
{
  "auto_detect_codecs": false,
  "manual_encoders": ["libx264"],
  "manual_decoders": ["h264"]
}
JSON
for NAME in slow-node fast-node; do
	start_worker "$NAME" RFFMPEG_CONFIG="$WORKER_CONFIG" \
		RFFMPEG_HEARTBEAT_INTERVAL=2s RFFMPEG_POLL_INTERVAL=300ms RFFMPEG_MAX_CONCURRENT=1
done
wait_worker slow-node
wait_worker fast-node

# The CLI probe uploads the input and dispatches the ffprobe work to a worker,
# so it needs a registered worker before it can run.
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	probe "$PROBE_MEDIA" >/dev/null 2>"$TMP_ROOT/probe.log"
FILE_ID="$(grep -oE 'Uploaded: .* -> [0-9a-f]+' "$TMP_ROOT/probe.log" | awk '{print $NF}' || true)"
[[ -n "$FILE_ID" ]] || fail "probe input file id not captured"

# worker_stats prints the panel as {"median":float,"workers":{name:{...}}}.
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
        "completed": health.get("completed_jobs", 0),
        "jobs_per_sec": health.get("jobs_per_sec", 0),
    }
print(json.dumps(out))
'
}

# panel_field STATS NAME FIELD prints one panel field (empty when absent).
panel_field() {
	python3 -c '
import json, sys
w = (json.loads(sys.argv[1])["workers"] or {}).get(sys.argv[2]) or {}
print(w.get(sys.argv[3], ""))
' "$1" "$2" "$3"
}

submit_probe() {
	local name="$1" attempt resp code
	local args="[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\",\"-preset\",\"veryslow\",\"-threads\",\"1\",\"-y\"]"
	for attempt in $(seq 1 60); do
		resp="$(curl -sS -X POST -H "Authorization: Bearer $AUTH_TOKEN" \
			-H 'Content-Type: application/json' \
			-d "{\"input_files\":[\"$FILE_ID\"],\"args\":$args,\"output_filename\":\"$name.mp4\"}" \
			-w '\n%{http_code}' "$(acceptance_server_url)/api/v1/jobs")"
		code="${resp##*$'\n'}"
		resp="${resp%$'\n'*}"
		if [[ "$code" == "200" ]]; then
			python3 -c 'import json,sys; print(json.loads(sys.argv[1])["job_id"])' "$resp"
			return 0
		fi
		sleep 1
	done
	fail "probe job submission kept failing (last HTTP $code)"
}

# inflight_jobs counts the non-terminal jobs of the newest page: the default
# per-client concurrency limit is 10, so the feeder must keep the queue small.
inflight_jobs() {
	api_get /api/v1/jobs | python3 -c '
import json, sys
jobs = json.load(sys.stdin)["jobs"]
print(sum(1 for j in jobs if j["status"] in ("pending", "queued", "running")))
'
}

# audit_rows SQL [ARGS...] prints matching rows as tab-separated values from the
# server's SQLite file. Read-only: the running server owns the database.
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

# pump_until_evicted TARGET BUDGET_S keeps both workers fed until TARGET is
# marked and the peer is not.
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
			submit_probe "sh6-pump-$n" >/dev/null
			n=$((n + 1))
			submit_probe "sh6-pump-$n" >/dev/null
		fi
		sleep 0.3
	done
}

pump_until_evicted slow-node 240 ||
	fail "slow-node was never evicted; the auth/error matrix needs a marked object"

STATS="$(worker_stats)"
SLOW_ID="$(panel_field "$STATS" slow-node id)"
FAST_ID="$(panel_field "$STATS" fast-node id)"
[[ -n "$SLOW_ID" && -n "$FAST_ID" ]] || fail "worker ids not found in the panel"
AUDIT_BEFORE="$(recovered_rows "$SLOW_ID")"
[[ "$AUDIT_BEFORE" == "0" ]] || fail "unexpected recovery audit rows before the clear: $AUDIT_BEFORE"

# post_clear TOKEN BODY -- sets CLEAR_STATUS / CLEAR_BODY.
post_clear() {
	local token="$1" body="$2" resp
	if [[ -n "$token" ]]; then
		resp="$(curl -sS -X POST -H "Authorization: Bearer $token" \
			-H 'Content-Type: application/json' -d "$body" \
			-w '\n%{http_code}' "$(acceptance_server_url)$CLEAR_PATH")"
	else
		resp="$(curl -sS -X POST -H 'Content-Type: application/json' -d "$body" \
			-w '\n%{http_code}' "$(acceptance_server_url)$CLEAR_PATH")"
	fi
	CLEAR_STATUS="${resp##*$'\n'}"
	CLEAR_BODY="${resp%$'\n'*}"
}

expect_status() {
	[[ "$CLEAR_STATUS" == "$1" ]] || fail "$2: HTTP $CLEAR_STATUS, want $1 (body: $CLEAR_BODY)"
}

expect_code() {
	python3 -c '
import json, sys
body = json.loads(sys.argv[1]) or {}
sys.exit(0 if body.get("code") == sys.argv[2] else 1)
' "$CLEAR_BODY" "$1" || fail "$2: body code != $1 (body: $CLEAR_BODY)"
}

# require_untouched LABEL -- the marked worker must still be marked and the
# out-of-band audit must be unchanged.
require_untouched() {
	local stats
	stats="$(worker_stats)"
	[[ "$(panel_field "$stats" slow-node evicted)" == "True" ]] ||
		fail "slow-node lost its mark $1"
	[[ "$(panel_field "$stats" fast-node evicted)" == "False" ]] ||
		fail "fast-node was wrongly marked $1"
	local rows
	rows="$(recovered_rows "$SLOW_ID")"
	[[ "$rows" == "$AUDIT_BEFORE" ]] ||
		fail "a rejected/no-op call wrote a recovery audit row $1 ($rows vs $AUDIT_BEFORE)"
}

# 401: no token and a wrong token are refused before the handler runs.
post_clear "" '{"worker":"slow-node"}'
expect_status 401 "clear without a token"
expect_code unauthorized "clear without a token"
require_untouched "after the tokenless attempt"

post_clear "wrong-token-value" '{"worker":"slow-node"}'
expect_status 401 "clear with a wrong token"
expect_code unauthorized "clear with a wrong token"
require_untouched "after the wrong-token attempt"

# 404: an object that does not exist.
post_clear "$AUTH_TOKEN" '{"worker":"no-such-worker"}'
expect_status 404 "clear of an unknown worker"
expect_code not_found "clear of an unknown worker"
require_untouched "after the unknown-object attempt"

# 400: no object given — empty body, empty field, blank field, malformed JSON.
post_clear "$AUTH_TOKEN" ''
expect_status 400 "clear without a body"
expect_code invalid_request "clear without a body"

post_clear "$AUTH_TOKEN" '{}'
expect_status 400 "clear without the worker field"
expect_code invalid_request "clear without the worker field"

post_clear "$AUTH_TOKEN" '{"worker":""}'
expect_status 400 "clear with an empty worker field"
expect_code invalid_request "clear with an empty worker field"

post_clear "$AUTH_TOKEN" 'not-json'
expect_status 400 "clear with a malformed body"
expect_code invalid_request "clear with a malformed body"
require_untouched "after the malformed requests"

# A worker that carries no mark: success, no state change, no audit row.
post_clear "$AUTH_TOKEN" '{"worker":"fast-node"}'
expect_status 200 "clear of an unmarked worker"
python3 -c '
import json, sys
body = json.loads(sys.argv[1])
assert body.get("cleared") is False, "cleared must be false for an unmarked worker: %r" % body
assert body.get("evicted") is False, "evicted must stay false: %r" % body
assert body.get("worker_id") == sys.argv[2], "wrong worker_id: %r" % body
' "$CLEAR_BODY" "$FAST_ID" || fail "unmarked-worker clear changed state (body: $CLEAR_BODY)"
FAST_RECOVERIES="$(recovered_rows "$FAST_ID")"
[[ "$FAST_RECOVERIES" == "0" ]] || fail "unmarked-worker clear wrote $FAST_RECOVERIES audit row(s)"

# Idempotent: the same call again stays a successful no-op.
post_clear "$AUTH_TOKEN" '{"worker":"fast-node"}'
expect_status 200 "second clear of an unmarked worker"
python3 -c '
import json, sys
body = json.loads(sys.argv[1])
assert body.get("cleared") is False and body.get("evicted") is False, "state changed on a no-op: %r" % body
' "$CLEAR_BODY" || fail "second no-op changed state (body: $CLEAR_BODY)"
FAST_RECOVERIES="$(recovered_rows "$FAST_ID")"
[[ "$FAST_RECOVERIES" == "0" ]] || fail "second no-op wrote $FAST_RECOVERIES audit row(s)"

require_untouched "after the no-op clears"

echo "PASS: 401/404 not_found/400 invalid_request rejected without touching the mark, unmarked clear is a no-op with no audit row"
