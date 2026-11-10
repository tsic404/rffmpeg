#!/usr/bin/env bash
# TC-observability-06: probe 输入拒绝的两条分支（AC-50）
# 判据：遍历路径 → HTTP 400 invalid_request、作业列表无新增行；
#       根内缺失直传路径 → 服务端不拒绝（不判断存在性），probe 落库后
#       500 probe_failed、作业列表新增一条失败行。
# 前置：共享文件系统的空间根已声明——未声明时绝对路径不属于任何形态，
#       在建作业前就是 400 invalid_request（C-123 / C-51）。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
# 声明空间根：缺失路径必须是形态合法的绝对路径，才能走到执行侧的探测失败。
PROBE_ROOT="$TMP_ROOT/probe-space"
mkdir -p "$PROBE_ROOT"
SERVER_EXTRA_FLAGS="--shared-fs-roots $PROBE_ROOT" start_server
wait_server_ready

API="$(acceptance_server_url)/api/v1"
AUTH=(-H "Authorization: Bearer $AUTH_TOKEN" -H "Content-Type: application/json")

jobs_count() {
	local n
	n="$(api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))')"
	printf '%s' "$n"
}

# --- Branch 1: traversal is a malformed path, rejected before any job ---------
BEFORE="$(jobs_count)"
BODY="$TMP_ROOT/traversal.json"
HTTP="$(curl -sS -o "$BODY" -w '%{http_code}' "${AUTH[@]}" -X POST "$API/probe" \
	-d '{"input":"/data/sub/../media.mp4"}')"
[[ "$HTTP" == "400" ]] || fail "traversal probe returned HTTP $HTTP, want 400 (body: $(cat "$BODY"))"
python3 - "$BODY" <<'PY' || fail "traversal rejection is not invalid_request"
import json, sys
d = json.load(open(sys.argv[1]))
assert d.get("code") == "invalid_request", d
PY
AFTER="$(jobs_count)"
[[ "$BEFORE" == "$AFTER" ]] || fail "rejected traversal probe created a job ($BEFORE -> $AFTER)"

# --- Branch 2: a missing shared-FS path reaches the Worker and fails there -----
start_worker "acceptance-worker-$PORT" RFFMPEG_SHARED_FS_ROOTS="$PROBE_ROOT"
wait_ready

MISSING="$PROBE_ROOT/does-not-exist-$PORT.mp4"
[[ ! -e "$MISSING" ]] || fail "fixture path unexpectedly exists: $MISSING"
BEFORE="$(jobs_count)"
BODY="$TMP_ROOT/missing-path.json"
HTTP="$(curl -sS -o "$BODY" -w '%{http_code}' "${AUTH[@]}" -X POST "$API/probe" \
	-d "{\"input\":\"$MISSING\"}")"
[[ "$HTTP" == "500" ]] || fail "missing direct path returned HTTP $HTTP, want 500 (body: $(cat "$BODY"))"
python3 - "$BODY" <<'PY' || fail "missing direct path failure is not probe_failed"
import json, sys
d = json.load(open(sys.argv[1]))
assert d.get("error") == "probe_failed", d
PY

AFTER="$(jobs_count)"
[[ "$AFTER" -eq $((BEFORE + 1)) ]] || fail "missing direct path did not create a probe job ($BEFORE -> $AFTER)"
api_get /api/v1/jobs >"$TMP_ROOT/jobs.json"
python3 - "$TMP_ROOT/jobs.json" "$MISSING" <<'PY' || fail "created probe job is not a failed row"
import json, sys
jobs = json.load(open(sys.argv[1]))["jobs"]
path = sys.argv[2]
job = next((j for j in jobs if path in (j.get("input_files") or [])), None)
assert job is not None, "no job carries the missing direct path"
assert job.get("status") == "failed", job
PY

echo "PASS: traversal -> 400 invalid_request with no job; missing direct path -> 500 probe_failed with one failed job"
