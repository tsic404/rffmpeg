#!/usr/bin/env bash
# TC-error-12: 缺 `-i` 的解析期拒绝（AC-51）
# 判据：`-c:v libx264 in.mp4`（`-q`）→ rc≠0；stderr 恰一行且含 `-i` 修复指引；
# 无终态行；作业列表无新增；无输出文件残留；原生 ffmpeg 同为非零。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

# 拒绝发生在提交之前，只需要 Server 应答健康检查；Worker 不参与。
build_binaries
start_server
wait_server_ready

job_count() {
	api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))'
}

RUN_DIR="$TMP_ROOT/run"
mkdir -p "$RUN_DIR"
LOG="$TMP_ROOT/cli.log"
BEFORE="$(job_count)"

set +e
(cd "$RUN_DIR" && "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" -q \
	-c:v libx264 in.mp4 >/dev/null 2>"$LOG")
RC=$?
set -e

expect_rc "$RC" 1 "missing -i rc"
[[ "$(wc -l < "$LOG")" -eq 1 ]] ||
	fail "stderr is not exactly one line: $(cat "$LOG")"
expect_in_file "$LOG" "no input file specified"
expect_in_file "$LOG" "-i"
grep -qE '^Job (failed|timed out|completed)|^Output saved' "$LOG" &&
	fail "rejection printed a terminal line: $(cat "$LOG")"
expect_no_panic "$LOG"

AFTER="$(job_count)"
[[ "$BEFORE" == "$AFTER" ]] || fail "rejection created a job ($BEFORE -> $AFTER)"
[[ ! -e "$RUN_DIR/in.mp4" ]] || fail "rejection left an output file"

# 原生 ffmpeg 同样把位置参数当输出并失败，且不留文件（AC-51 第三条）。
set +e
(cd "$RUN_DIR" && ffmpeg -loglevel error -c:v libx264 in.mp4 >/dev/null 2>"$TMP_ROOT/native.log")
NATIVE_RC=$?
set -e
(( NATIVE_RC != 0 )) || fail "native ffmpeg accepted the same invocation"
[[ ! -e "$RUN_DIR/in.mp4" ]] || fail "native ffmpeg left an output file"

echo "PASS: rc=1, one line naming the -i fix, no job, no output; native rc=$NATIVE_RC"
