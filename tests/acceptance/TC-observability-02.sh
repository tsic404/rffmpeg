#!/usr/bin/env bash
# TC-observability-02: probe 拒绝路径（AC-39）
# 判据：rc=1；stderr 单行且与提交拒绝同形；该分支作业列表无新增行。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
wait_server_ready

MEDIA="$(make_media probe-none.mp4 2)"
LOG="$TMP_ROOT/probe.log"
BEFORE="$(api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))')"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	probe "$MEDIA" >/dev/null 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 1 "probe-without-worker rc"

WANT="Error probing file: probe failed [worker_unavailable]: No workers available. Please ensure at least one worker is registered and online."
grep -qxF "$WANT" "$LOG" || fail "stderr line mismatch; want exactly: $WANT"

AFTER="$(api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))')"
[[ "$BEFORE" == "$AFTER" ]] || fail "rejected probe created a job ($BEFORE -> $AFTER)"

echo "PASS: rc=1, single exact rejection line, no job created"
