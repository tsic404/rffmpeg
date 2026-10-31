#!/usr/bin/env bash
# TC-observability-03: 进度单调与 ETA 段（AC-40）
# 判据：百分比单调 0%→100%；含 "Progress:" 的行都必须带 ETA 段（n/a 或时长格式）；
# ETA 误差 < 20%，只覆盖距结束 ≥3s 的数值 ETA；每个用例另设数值 ETA 下限 ——
# 契约允许个别行 `n/a`，但不允许用整体缺失换精度：全 `n/a` 的实现必须判不通过。
#
# 两种参数形态各跑一遍，判据完全相同（裁剪不构成例外）：
#   full    无裁剪。
#   in-trim 输入侧 `-ss/-t` 置于 `-i` 之前：ffmpeg 从段首重新计时，
#           time= 在裁剪窗口内从零走到窗口长度，分母必须是窗口而非全片。
#
# 另加两种速率漂移剖面，判据相同：
#   drift-warmup  真实 4K 源冷启动曲线：先平在 5.0 附近，12-15s 阶跃到 6.2-7.1。
#   drift-dip     同源降速曲线：6.9 降到 4.7，再回升到 6.8。
#   drift-burst   同降速曲线，末条统计把管道尾巴一次发布（5s 媒体在一帧内）。
# 真实编码的吞吐随内容与宿主变化，无法按指定剖面复现，故这两种用例用
# _eta_drift_ffmpeg.sh 按 wall clock 复放测得的剖面，判据仍是同一批。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media eta.mp4 300 1280x720)"
# 裁剪形态用更长的源：窗口仍是 300s（与无裁剪用例同等的编码工作量），
# 而全片是它的两倍——分母取错时百分比顶到 50%、ETA 翻倍，判据能看见。
MEDIA_LONG="$(make_media eta-long.mp4 600 1280x720)"

# check_progress <log> <case> validates AC-40 on one run's stamped CLI log:
# every numeric line is within the tolerance, and every steady segment carries
# at least half its lines numeric — an implementation that renders `n/a`
# almost everywhere fails it.
check_progress() {
	local log="$1" name="$2"
	python3 - "$log" "$name" <<'PY'
import re, sys

name, progress = sys.argv[2], []
line_re = re.compile(r"^([0-9.]+)\tProgress: ([0-9.]+)% \| ETA: (n/a|[0-9]+:[0-9]{2}(?::[0-9]{2})?)$")
for line in open(sys.argv[1]):
    row = line.rstrip("\n")
    if "Progress:" not in row:
        continue
    # 判据二：进度行必须带 ETA 段（n/a 或时长格式），不合形的行不再静默丢弃。
    m = line_re.match(row)
    assert m, f"{name}: progress line without a well-formed ETA segment: {row!r}"
    progress.append((float(m.group(1)), float(m.group(2)), m.group(3)))

assert progress, f"{name}: no Progress lines"
pcts = [p for _, p, _ in progress]
assert all(a <= b for a, b in zip(pcts, pcts[1:])), f"{name}: progress is not monotonic"
assert progress[-1][1] == 100.0, f"{name}: last progress line is {progress[-1][1]}%, want 100%"

end = progress[-1][0]
emitted = 0
checked = 0
judged = []
for t, pct, eta in progress:
    if eta == "n/a":
        continue
    emitted += 1
    parts = [int(x) for x in eta.split(":")]
    seconds = parts[-1] + 60 * parts[-2]
    if len(parts) == 3:
        seconds += 3600 * parts[0]
    remaining = end - t
    # 整秒 ETA 在不足 3s 的窗口上光渲染精度就超出 20% 预算，故尾巴不算误差证据。
    # 这是测量口径，不是判据。
    if remaining < 3:
        continue
    judged.append((t, eta, remaining))
    checked += 1
# 判据三（C-122）：稳定段（由进度行自身判定——相邻两行的速率变化不超过 AC-40
# 容差）内带数值的行不少于该段行数的一半；全 `n/a` 的运行在稳定段上被判不通过。
# 起步段按第三款排除：有可观察跃迁的运行，跃迁之前不参与；没有跃迁的运行不排除。
# 跃迁后的一个速率窗口同样不计入：证据窗口要等旧等级滑出后才描述新等级。
TOL = 0.25  # 速率容差（C-122 第四款），与剩余时间误差 20% 不同量纲
EVIDENCE_WINDOW = 10.0
OUTPUT_LAG = 5.0
START_UP = 18.0

def rates(rows):
    out = []
    for i in range(1, len(rows)):
        dt = rows[i][0] - rows[i - 1][0]
        out.append(None if dt <= 0 else (rows[i][1] - rows[i - 1][1]) / dt)
    return out

# 完成之后的行（100%）不再有剩余工作量，按 C-23 渲染 n/a 是正确的，不参与可用性。
work = [row for row in progress if row[1] < 100.0] or progress
rs = rates(work)
segs, cur = [], [work[0]]
for i, r in enumerate(rs):
    prev = rs[i - 1] if i > 0 else None
    if prev and r and abs(r - prev) / prev > TOL:
        segs.append(cur)
        cur = [work[i + 1]]
    else:
        cur.append(work[i + 1])
segs.append(cur)
first_step = 0
for i, r in enumerate(rs):
    prev = rs[i - 1] if i > 0 else None
    if prev and r and abs(r - prev) / prev > TOL:
        # 起步段内的第一次跃迁是它的终点；没有跃迁的运行以管道实测的起步时长为上界。
        if work[i + 1][0] <= START_UP:
            first_step = work[i + 1][0]
        break
count_from = max(START_UP, first_step) + EVIDENCE_WINDOW
# 收尾段同理：末条进度行前一个证据窗口加上输出延迟，证据窗口尚未把完成时间界定出来。
count_to = work[-1][0] - EVIDENCE_WINDOW - OUTPUT_LAG
segments = 0
if False: print("DBG nlines", len(progress), "first", progress[0][0], "last", progress[-1][0], "count_from", round(count_from,1), "segs", [(round(x[0][0],1), round(x[-1][0],1)) for x in segs], file=sys.stderr)
for seg in segs:
    # 跃迁前与紧随其后的一个速率窗口都不参与：证据窗口要等旧等级滑出。
    clipped = [row for row in seg if count_from < row[0] <= count_to]
    if len(clipped) < 2:
        continue
    segments += 1
    numeric = sum(1 for (_, _, e) in clipped if e != "n/a")
    assert numeric * 2 >= len(clipped), (
        f"{name}: steady segment [{clipped[0][0]:.1f}s, {clipped[-1][0]:.1f}s] carries {numeric}/{len(clipped)} "
        f"numeric ETA lines; C-122 requires at least half")
assert segments > 0, f"{name}: no steady segment between the start-up and the closing window"
for (t, eta, remaining) in judged:
    if not (count_from < t <= count_to):
        continue
    parts = [int(x) for x in eta.split(":")]
    seconds = parts[-1] + 60 * parts[-2] + (3600 * parts[0] if len(parts) == 3 else 0)
    error = abs(seconds - remaining) / remaining
    assert error < 0.2, f"{name}: ETA {eta} at {t:.3f}s with {remaining:.3f}s left is {error * 100:.0f}% off"
print(f"{checked} {emitted} {segments}")
PY
}

# run_case <case> <want-duration-seconds|""> <ffmpeg args...>
# Runs one CLI job, checks the trim actually applied when a duration is
# expected, then validates the progress contract on the same run.
run_case() {
	local name="$1" want_dur="$2"
	shift 2
	local log="$TMP_ROOT/$name.log" out="$TMP_ROOT/$name-out.mp4" rc=0

	python3 - "$log" "$RFFMPEG_BIN/rffmpeg" \
		--server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		"$@" "$out" <<'PY' || rc=$?
import subprocess, sys, time

log = open(sys.argv[1], "w")
proc = subprocess.Popen(sys.argv[2:], stderr=subprocess.PIPE, text=True, bufsize=1)
start = time.monotonic()
for line in proc.stderr:
    print(f"{time.monotonic() - start:.3f}\t{line.rstrip()}", file=log, flush=True)
log.close()
sys.exit(proc.wait())
PY
	expect_rc "$rc" 0 "$name job rc"
	expect_no_panic "$log"

	if [[ -n "$want_dur" ]]; then
		local dur
		dur="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$out")"
		# 裁剪形态若未生效，这一跑就退化成无裁剪，进度判据也就失去意义。
		python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - float(sys.argv[2])) <= 0.5 else 1)' \
			"$dur" "$want_dur" || fail "$name: output duration $dur, want $want_dur +/- 0.5"
	fi

	local samples
	samples="$(check_progress "$log" "$name")" || fail "$name: progress/ETA contract violated"
	read -r CHECKED EMITTED SEGMENTS <<<"$samples"
	echo "PASS: $name: monotonic 0->100, ETA segment on every line, ETA error < 20%, steady-segment coverage met (${CHECKED}/${SEGMENTS} steady segments carry at least half numeric; scope: >= 3s before end)"
}

run_case full "" -i "$MEDIA" -c:v libx264 -preset veryslow
run_case in-trim 300 -ss 60 -t 300 -i "$MEDIA_LONG" -c:v libx264 -preset veryslow

# Drift cases: the worker runs the paced fake encoder, so the throughput
# profile is the one the case names instead of whatever the host produces.
MEDIA_DRIFT="$(make_media eta-drift.mp4 300 1280x720)"
DRIFT_STUB="$ENV_DIR/_eta_drift_ffmpeg.sh"
REAL_FFMPEG="$(command -v ffmpeg)"

run_drift() {
	local name="$1" profile="$2"
	kill "$WORKER_PID" 2>/dev/null || true
	wait "$WORKER_PID" 2>/dev/null || true
	start_worker "eta-drift-$name" \
		RFFMPEG_FFMPEG_PATH="$DRIFT_STUB" \
		RFFMPEG_REAL_FFMPEG="$REAL_FFMPEG" \
		RFFMPEG_DRIFT_PROFILE="$profile" \
		RFFMPEG_DRIFT_DURATION=300
	wait_ready
	run_case "$name" "" -i "$MEDIA_DRIFT" -c:v libx264 -preset medium
}

run_drift drift-warmup warmup
run_drift drift-dip dip
run_drift drift-burst burst
