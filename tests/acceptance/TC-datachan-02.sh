#!/usr/bin/env bash
# TC-datachan-02: 不可达 URL（AC-33）
# 判据：终态分类为 INPUT_UNREACHABLE。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

URL="http://127.0.0.1:9/definitely-unreachable.mp4"
LOG="$TMP_ROOT/cli.log"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$URL" "$TMP_ROOT/out.mp4" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 1 "unreachable-URL rc"

grep -qF 'Job failed: [INPUT_UNREACHABLE]' "$LOG" || fail "missing INPUT_UNREACHABLE terminal line"
JOB="$(job_id_from_log "$LOG")"
api_get "/api/v1/jobs/$JOB" |
	python3 -c '
import json, sys
job = json.load(sys.stdin)["job"]
assert job["failure_type"] == "INPUT_UNREACHABLE", job["failure_type"]
assert job["retryable"] is False, job["retryable"]
' || fail "job record is not INPUT_UNREACHABLE/retryable=false"

echo "PASS: rc=1, INPUT_UNREACHABLE classified"
