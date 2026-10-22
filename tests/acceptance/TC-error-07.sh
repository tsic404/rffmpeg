#!/usr/bin/env bash
# TC-error-07: 提交时有 worker、随后崩溃（AC-17，Server 侧 sweep 分支）
# 判据：迁回 pending 后 (120s, 150s] 内 fail；分类 WORKER_CRASH、stderr 逐字；
# 从提交到 CLI 返回失败的壁钟 ≤ 300s。使用默认心跳/巡检/饥饿阈值。
#
# 判定点是 sweep 自身的迁移→终态转移，取作业记录的 created_at（最后一次迁回
# pending 时刷新）与 finished_at（sweep 写入）之差；客户端轮询时长只作诊断，
# 不作为窗口判据。同时要求该锚点的 wake 武装/触发与判定路径自洽。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media sweep120.mp4 120 1280x720)"
LOG="$TMP_ROOT/cli.log"
OUT="$TMP_ROOT/out.mp4"
START=$(now_ns)

timeout 320 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -preset veryslow -threads 2 "$OUT" >/dev/null 2>"$LOG" &
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

kill -9 "$(worker_pid "acceptance-worker-$PORT")" || fail "worker pid not recorded"

PENDING_AT=""
FAIL_AT=""
DEADLINE=$((SECONDS + 300))
while (( SECONDS < DEADLINE )); do
	STATUS="$(job_field "$JOB" status 2>/dev/null || true)"
	if [[ "$STATUS" == "pending" && -z "$PENDING_AT" ]]; then
		PENDING_AT=$(now_ns)
	fi
	if [[ "$STATUS" == "failed" ]]; then
		FAIL_AT=$(now_ns)
		break
	fi
	sleep 1
done
wait "$CLI_PID" 2>/dev/null || true

[[ -n "$PENDING_AT" ]] || fail "job was never migrated back to pending"
[[ -n "$FAIL_AT" ]] || fail "job did not fail within 300s"

P2F_MS=$(( (FAIL_AT - PENDING_AT) / 1000000 ))
TOTAL_MS=$(( (FAIL_AT - START) / 1000000 ))
(( TOTAL_MS <= 300000 )) || fail "submit->failure took ${TOTAL_MS}ms, want <= 300s"

JOB_JSON="$(api_get "/api/v1/jobs/$JOB")" || fail "failed to read job record"
# created_at / finished_at are the sweep's own transition timestamps.
# Microseconds keep the strict lower bound honest: the tick can judge a few
# hundred microseconds past the 120s cutoff, which integer milliseconds would
# truncate back to exactly 120000.
read -r ANCHOR SWEEP_US < <(
	python3 -c '
import datetime, json, sys
job = json.load(sys.stdin)["job"]
parse = lambda s: datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))
anchor, finished = parse(job["created_at"]), parse(job["finished_at"])
print(job["created_at"], (finished - anchor) // datetime.timedelta(microseconds=1))
' <<<"$JOB_JSON"
) || fail "job record lacks created_at/finished_at"
(( SWEEP_US > 120000000 && SWEEP_US <= 150000000 )) ||
	fail "sweep judged ${SWEEP_US}us after the crash migration, want (120s, 150s]"

# The targeted wake must be armed for the job's pending anchor, and a verdict
# past that wake's deadline must carry a matching wake-fired line: a tick that
# silently makes up for a vanished wake fails here instead of passing on the
# terminal state alone. deadline_due is a legitimate third path (the arm found
# the deadline already past) and has its own marker.
EVIDENCE="$(
	python3 - "$ANCHOR" "$SERVER_LOG" <<'PY'
import datetime, re, sys

anchor, log_path = sys.argv[1], sys.argv[2]
lines = open(log_path, encoding="utf-8", errors="replace").read().splitlines()


def parse(value):
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))


armed, fired, verdicts = [], [], []
for line in lines:
    m = re.search(r"starvation wake armed: anchor=(\S+) deadline=(\S+)", line)
    if m:
        if m.group(1) == anchor:
            armed.append(parse(m.group(2)))
        continue
    m = re.search(r"starvation wake fired at (\S+)", line)
    if m:
        fired.append(parse(m.group(1)))
        continue
    m = re.search(r"starvation verdict via ([a-z_]+):.*judged_at=([^\s)]+)", line)
    if m:
        verdicts.append((m.group(1), parse(m.group(2))))

if not verdicts:
    sys.exit("server log has no starvation verdict line")
trigger, judged = verdicts[-1]

if trigger == "deadline_due":
    if not any(f"already due (anchor={anchor})" in line for line in lines):
        sys.exit("verdict via deadline_due without the matching due-at-arm line")
    print(f"path=deadline_due judged={judged.isoformat()}")
    sys.exit(0)

if not armed:
    sys.exit(f"wake was never armed for the pending anchor {anchor}")
deadline = armed[-1]
if judged > deadline and not fired:
    sys.exit(f"verdict at {judged.isoformat()} is past the wake deadline {deadline.isoformat()} but no wake fired")
if fired and fired[-1] < deadline:
    sys.exit(f"wake fired at {fired[-1].isoformat()} before its deadline {deadline.isoformat()}")
if trigger == "wake" and not fired:
    sys.exit("verdict via wake without a wake-fired line")
fired_text = fired[-1].isoformat() if fired else "none"
print(f"path={trigger} deadline={deadline.isoformat()} fired={fired_text} judged={judged.isoformat()}")
PY
)" || fail "starvation path evidence is inconsistent (see server log)"

python3 -c '
import json, sys
job = json.load(sys.stdin)["job"]
assert job["status"] == "failed", job["status"]
assert job["failure_type"] == "WORKER_CRASH", job["failure_type"]
assert job["retryable"] is True, job["retryable"]
' <<<"$JOB_JSON" || fail "job record is not failed/WORKER_CRASH/retryable=true"

WANT="Job failed: [WORKER_CRASH] worker crashed (heartbeat timeout) and no surviving worker to take over the job"
grep -qxF "$WANT" "$LOG" || fail "stderr line mismatch; want exactly: $WANT"

echo "PASS: WORKER_CRASH, sweep=$(( SWEEP_US / 1000 ))ms (${SWEEP_US}us), client-observed=${P2F_MS}ms, total=${TOTAL_MS}ms, ${EVIDENCE}"
