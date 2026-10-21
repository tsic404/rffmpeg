#!/usr/bin/env bash
# TC-security-06: 作业执行预算生效与清理（AC-31）
#
# 可观察判据（docs/03-acceptance.md AC-31）:
#   - rc≠0
#   - CLI 壁钟 ≤ --timeout 预算 + 10s 收敛余量
#   - 不产生部分输出（输出文件不存在）
#   - Worker 无残留 ffmpeg 进程
#
# 本实现还断言 Worker 侧终态上报不被 stderr 排空阻塞（终端刷写有界，
# 见 pkg/worker/stderr_batcher.go terminalFlushBudget）：这是 AC-31 收敛
# 余量成立的关键——终态判定不得等 stderr 通道排空。

set -euo pipefail
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

# 生成足够大、足够慢的输入，使 ffmpeg 执行时长超过 5s 预算（缓存禁用，
# 超时路径必须触发）。
MEDIA="$MEDIA_DIR/test-large.mp4"
if [[ ! -f "$MEDIA" ]]; then
	ffmpeg -loglevel error -y -f lavfi \
		-i testsrc=duration=120:size=1920x1080:rate=30 \
		-c:v libx264 "$MEDIA"
fi
DURATION="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$MEDIA")"
[[ "$DURATION" == 120.* ]] || { echo "unexpected media duration: $DURATION" >&2; exit 1; }

# busybox date has no %N: `date +%s%N` silently degrades to whole seconds,
# WALL_MS collapses to 0 and the 15s criterion can never fail. Resolve the
# clock once, in this parent shell — a subshell assignment would be lost on
# every $(now_ns) call. Prefer python's monotonic clock (immune to NTP steps,
# Python >= 3.3); date +%s%N is only a fallback when python3 is missing.
# Accept exactly 10-digit seconds + 9-digit %N; pure-second or short %N output
# would scale WALL_MS down a thousandfold and reopen the false-positive.
if command -v python3 >/dev/null; then
	now_ns() { python3 -c 'import time; print(int(time.monotonic() * 1e9))'; }
elif command -v date >/dev/null && [[ "$(date +%s%N)" =~ ^[0-9]{19}$ ]]; then
	now_ns() { date +%s%N; }
else
	echo "FATAL: neither python3 (monotonic clock) nor a nanosecond-capable date is available" >&2
	exit 1
fi

OUT="$TMP_ROOT/sec6b.mp4"
START_NS=$(now_ns)
set +e
"$RFFMPEG_BIN/rffmpeg" \
	--server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	--timeout 5s \
	-i "$MEDIA" -c:v libx264 -preset veryslow \
	"$OUT" 2> "$TMP_ROOT/cli.log"
RC=$?
set -e
END_NS=$(now_ns)
WALL_MS=$(( (END_NS - START_NS) / 1000000 ))
WALL_DISPLAY=$(printf '%d.%03ds' $((WALL_MS / 1000)) $((WALL_MS % 1000)))

# rc≠0
[[ $RC -ne 0 ]] || { echo "FAIL: rc=$RC, want != 0" >&2; exit 1; }

# 壁钟 ≤ 预算 + 10s 收敛余量
if (( WALL_MS > 15 * 1000 )); then
	echo "FAIL: wall=${WALL_MS}ms > 15000ms (5s budget + 10s margin)" >&2
	exit 1
fi

# 分类与终态行
grep -q 'Job timed out: \[TIMEOUT\] ffmpeg command timed out after' "$TMP_ROOT/cli.log" \
	|| { echo "FAIL: missing TIMEOUT terminal line in cli.log" >&2; cat "$TMP_ROOT/cli.log" >&2; exit 1; }

# 不产生部分输出
if [[ -e "$OUT" ]]; then
	echo "FAIL: partial output $OUT exists" >&2
	exit 1
fi

# Worker 无残留 ffmpeg 子进程：只按本 worker 启动的 PID 取证
if kill -0 "$WORKER_PID" 2>/dev/null; then
	CHILDREN=$(pgrep -P "$WORKER_PID" -f ffmpeg 2>/dev/null || true)
	if [[ -n "$CHILDREN" ]]; then
		echo "FAIL: residual ffmpeg processes under worker: $CHILDREN" >&2
		exit 1
	fi
fi

echo "PASS: rc=$RC wall=${WALL_DISPLAY} <= 15s, TIMEOUT classified, no partial output, no residual ffmpeg"
