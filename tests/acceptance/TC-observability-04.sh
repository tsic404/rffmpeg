#!/usr/bin/env bash
# TC-observability-04: Worker 心跳面板字段契约（AC-41）
# 判据：恒在字段全部出现（真 0 也在）；仅活跃作业可省略；最后心跳在顶层。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

api_get /api/v1/workers |
	python3 -c '
import json, sys

w = json.load(sys.stdin)["workers"][0]
assert "last_heartbeat" in w, "last_heartbeat must be at the worker top level"

health = w["health"]
required = [
    "status", "gpu_metrics_valid", "jobs_per_sec", "last_seen",
    "ewma_jobs_per_sec", "completed_jobs", "gpu_util_percent", "gpu_mem_used_mb",
]
missing = [k for k in required if k not in health]
assert not missing, "health keys missing (even true zero must be present): %r" % missing

assert isinstance(health["gpu_util_percent"], (int, float)), "gpu_util_percent must be numeric"
assert isinstance(health["gpu_mem_used_mb"], (int, float)), "gpu_mem_used_mb must be numeric"

if "active_jobs" in health:
    assert isinstance(health["active_jobs"], list), "active_jobs must be a list when present"
' || fail "heartbeat panel field contract violated"

echo "PASS: all constant health fields present, last_heartbeat at top level"
