#!/usr/bin/env bash
# TC-selfheal-02: 迁移事件 API 字段契约（AC-44、F8 负向）
# 判据：外层键为 events；每事件 targets[] 为每作业粒度；缺键用 has/null 判断；
# 迁移事件表无 target 列（事件对象顶层不得出现 target 字段）。
set -euo pipefail
PORT="${PORT:-18999}"
SERVER_EXTRA_FLAGS="--worker-heartbeat-timeout 10s --worker-health-check-interval 2s"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker worker-a RFFMPEG_HEARTBEAT_INTERVAL=2s
wait_worker worker-a

MEDIA="$(make_media migrate.mp4 120 1280x720)"
LOG="$TMP_ROOT/cli.log"
OUT="$TMP_ROOT/migrated.mp4"

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
kill -9 "$(worker_pid worker-a)" || fail "worker-a pid not recorded"

set +e
wait "$CLI_PID"
RC=$?
set -e
expect_rc "$RC" 0 "migrated job rc"

api_get /api/v1/migrations >"$TMP_ROOT/events.json"
EVENT_ID="$(python3 - "$TMP_ROOT/events.json" "$JOB" <<'PY'
import json, sys

data = json.load(open(sys.argv[1]))
assert set(data) == {"events"}, sorted(data)
events = data["events"]
assert events, "no migration events"
e = next((x for x in events if x.get("worker_name") == "worker-a"), None)
assert e is not None, "no migration event for worker-a"
job_id = sys.argv[2]

for key in ("id", "timestamp", "worker_id", "worker_name", "reason", "retry_count", "job_ids", "jobs_migrated"):
    assert key in e, f"missing event field: {key}"
assert isinstance(e["job_ids"], list) and e["job_ids"], "job_ids must be a JSON array"
assert e["reason"] == "heartbeat_timeout", e["reason"]
assert e["worker_name"] == "worker-a", e["worker_name"]

# F8: per-job targets are rows of their own table, never columns on the event.
for key in ("target", "target_worker_id", "target_worker_name"):
    assert key not in e, f"{key} must not appear on a migration event"

if "targets" in e:
    assert isinstance(e["targets"], list), "targets must be an array"
    for t in e["targets"]:
        assert isinstance(t, dict), "targets entries must be per-job objects"
        assert "job_id" in t and "retry_count" in t, t
        for key in ("target_worker_id", "target_worker_name"):
            if key in t:
                assert isinstance(t[key], str) and t[key], f"{key} present but empty"
    assert any(t.get("job_id") == job_id for t in e["targets"]), "targets lack the migrated job"

print(e["id"])
PY
)" || fail "migration events contract violated"
[[ -n "$EVENT_ID" ]] || fail "migration event id missing"

api_get "/api/v1/migrations/$EVENT_ID" |
	python3 -c '
import json, sys
data = json.load(sys.stdin)
assert set(data) == {"event"}, sorted(data)
assert data["event"]["id"] == sys.argv[1], data["event"]["id"]
' "$EVENT_ID" || fail "single migration event shape violated"

echo "PASS: events/targets shape correct, no target column on the event, id=$EVENT_ID"
