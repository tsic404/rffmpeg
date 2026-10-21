#!/usr/bin/env bash
# TC-error-06: 提交时即无存活 worker（AC-16，fail-fast 分支）
# 判据：秒级 rc≠0；stderr 逐字匹配；作业列表无新增行（不产生 job）。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
wait_server_ready

MEDIA="$(make_media noworker.mp4 2)"
LOG="$TMP_ROOT/cli.log"
BEFORE="$(api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))')"

START=$(now_ns)
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" "$TMP_ROOT/out.mp4" >/dev/null 2>"$LOG"
RC=$?
set -e
WALL_MS=$(( ($(now_ns) - START) / 1000000 ))

expect_rc "$RC" 1 "no-worker rc"
(( WALL_MS < 15000 )) || fail "fail-fast took ${WALL_MS}ms, want seconds-level"

WANT="Error submitting job: job submission failed [worker_unavailable]: No workers available. Please ensure at least one worker is registered and online."
grep -qxF "$WANT" "$LOG" || fail "stderr line mismatch; want exactly: $WANT"

AFTER="$(api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))')"
[[ "$BEFORE" == "$AFTER" ]] || fail "rejected submission created a job ($BEFORE -> $AFTER)"

echo "PASS: rc=1 in ${WALL_MS}ms, exact stderr, no job created"
