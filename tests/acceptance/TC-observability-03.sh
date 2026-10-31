#!/usr/bin/env bash
# TC-observability-03: 进度单调与 ETA 段（AC-40）
#
# 判据：百分比单调 0%→100%；含 "Progress:" 的行都必须带 ETA 段（n/a 或时长格式）；
# 带数值行的误差 <20%（真值不在该行推出的区间内者只计覆盖率）；
# 稳定段内带数值行不少于该段一半；无稳定段时改判运行级下限 15%。
# 覆盖率的分母按 C-122 第六款排除：起步段、起步段后一个证据窗口、
# 跃迁后一个证据窗口、末条进度行之前的一个证据窗口。
#
# 两种参数形态各跑一遍，判据完全相同（裁剪不构成例外）：
#   full    无裁剪。
#   in-trim 输入侧 `-ss/-t` 置于 `-i` 之前：ffmpeg 从段首重新计时，
#           time= 在裁剪窗口内从零走到窗口长度，分母必须是窗口而非全片。
#
# 另加三种速率漂移剖面，判据相同：
#   drift-warmup  真实 4K 源冷启动曲线：先平在 5.0 附近，12-15s 阶跃到 6.2-7.1。
#   drift-dip     同源降速曲线：6.9 降到 4.7，再回升到 6.8。
#   drift-burst   同降速曲线，末条统计把管道尾巴一次发布（5s 媒体在一帧内）。
# 真实编码的吞吐随内容与宿主变化，无法按指定剖面复现，故这三种用例用
# _eta_drift_ffmpeg.sh 按 wall clock 复放测得的剖面，判据仍是同一批。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

# Media seconds the progress percentages are measured against. Every case runs
# a 300s window: the trimmed case re-times inside its window, and the drift
# cases replay a 300s input.
MEDIA_SECONDS=300

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media eta.mp4 300 1280x720)"
# 裁剪形态用更长的源：窗口仍是 300s（与无裁剪用例同等的编码工作量），
# 而全片是它的两倍——分母取错时百分比顶到 50%、ETA 翻倍，判据能看见。
MEDIA_LONG="$(make_media eta-long.mp4 600 1280x720)"

# check_progress <log> <case> <media-seconds> validates AC-40 on one run's
# stamped CLI log.
check_progress() {
	local log="$1" name="$2" media_seconds="$3"
	python3 - "$log" "$name" "$media_seconds" <<'PY'
import math
import re
import sys

name, media_seconds = sys.argv[2], float(sys.argv[3])
progress = []
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

TOL = 0.25    # 速率容差（C-122 第四款），与剩余时间误差 20% 不同量纲
BAND = 0.15   # 累计带宽
W = 10.0      # 证据窗口（采样/管道属性）
LAG = 5.0     # 输出延迟（媒体秒）
FINALIZE = 0.75  # 收尾界（墙钟秒）
START_UP = 18.0  # 管道实测起步时长上界（墙钟秒）

def media_rem(pct):
    return (100.0 - pct) / 100.0 * media_seconds

def line_rate(rows, i):
    if i <= 0:
        return None
    dt = rows[i][0] - rows[i - 1][0]
    if dt <= 0:
        return None
    return (rows[i][1] - rows[i - 1][1]) / 100.0 * media_seconds / dt

# 完成之后的行（100%）不再有剩余工作量，按 C-23 渲染 n/a 是正确的，不参与可用性。
work = [row for row in progress if row[1] < 100.0] or progress
# 实现侧的速率序列是 2s 分块均值。稳定段、跃迁与起步边界都在这条序列上
# 判定，判据与实现同源；逐行百分比内容抖动不再被当成跃迁（C-122 第四款
# 在实现侧就是 2s 分块的粒度）。
def chunk_rate(i, base=0):
    # 块的首行取距块起点（末行 − 2s）最近的一行（C-122）；窗口起点所在的
    # 首块截短到窗口起点（base）。块内首末两行的实测墙钟差 <1s 或没有进度差
    # 时不产生块速率。
    if i <= base:
        return None
    start = work[i][0] - 2.0
    j = min(range(base, i), key=lambda k: abs(work[k][0] - start))
    dt = work[i][0] - work[j][0]
    dp = work[i][1] - work[j][1]
    if dt < 1.0 or dp <= 0:
        return None
    return dp / 100.0 * media_seconds / dt

chunk_rates = [chunk_rate(i) for i in range(len(work))]
segs, cur, cur_start = [], [work[0]], 0
chunk_breaks, chunk_steps = [], []
for i in range(1, len(work)):
    rate, prev = chunk_rates[i], chunk_rates[i - 1]
    step = bool(prev and rate and abs(rate / prev - 1) > TOL)
    trs = [chunk_rates[j] for j in range(cur_start + 2, i + 1) if chunk_rates[j]]
    band = len(trs) >= 2 and max(trs) / min(trs) - 1 > BAND
    if step:
        chunk_steps.append(work[i][0])
    if step or band:
        segs.append(cur)
        cur = [work[i]]
        cur_start = i
        chunk_breaks.append(work[i][0])
    else:
        cur.append(work[i])
segs.append(cur)
segs = [seg for seg in segs if len(seg) >= 2]

# 起步段边界：进程启动到速率在起步之后重新稳定为止。首次速率阶跃落在管道
# 起步上界内时以它为界；没有跃迁的运行以管道实测起步时长为上界。
# CLI 时钟包含作业排队时间，进程启动只能取首个进度行：它是 ffmpeg 开始发布
# 统计行的那一行，此前的时间不是编码过程。取首个进度行把排除区间右端整体
# 后移约一个采样周期，方向是放宽（分母与段内判据略松），代价已记录。
first = work[0][0]
startup_end = first + START_UP
for b in chunk_steps:
    if b - first <= START_UP:
        startup_end = b
    break
last = progress[-1][0]
excluded = [(0.0, startup_end + W)]
excluded += [(b, b + W) for b in chunk_breaks]
excluded.append((last - W, float("inf")))

def counted(rows):
    return [row for row in rows if not any(a < row[0] <= b for a, b in excluded)]

# 判据三：有稳定段时段内带数值行 ≥ 一半；否则改判运行级下限 15%。
segments = 0
for seg in segs:
    clipped = counted(seg)
    if len(clipped) < 2:
        continue
    segments += 1
    numeric = sum(1 for (_, _, e) in clipped if e != "n/a")
    assert numeric * 2 >= len(clipped), (
        f"{name}: steady segment [{clipped[0][0]:.1f}s, {clipped[-1][0]:.1f}s] carries "
        f"{numeric}/{len(clipped)} numeric ETA lines; C-122 requires at least half")
# 判据二：跃迁段不给值（C-122 第二款）。断言锁「窗口内一次超过容差的
# 阶跃」：窗口按实现同一序列重算（2s 分块），相邻块 |比−1| > 0.25 的
# 断点之后一个证据窗口内不得给值。累计带宽那条是稳定段的段界判据，由下面
# 的覆盖率核算判定。
transition_rows = 0
for (t, pct, eta) in progress:
    if eta == "n/a":
        continue
    if any(t - W < b <= t for b in chunk_steps):
        transition_rows += 1
        print(f"{name}: ETA {eta} at {t:.3f}s while a transition still shares the window",
              file=sys.stderr)
assert transition_rows == 0, f"{name}: {transition_rows} numeric ETA lines on transition windows"
if segments == 0:
    floor = counted(work)
    numeric = sum(1 for (_, _, e) in floor if e != "n/a")
    assert len(floor) > 0, (
        f"{name}: the exclusions removed every progress row; an empty denominator "
        f"cannot prove availability")
    assert numeric * 100 >= 15 * len(floor), (
        f"{name}: no steady segment and only {numeric}/{len(floor)} numeric ETA lines; "
        f"C-122 run-level floor is 15% of the excluded-window denominator")
# 全 n/a 的运行不通过：任何工况至少要有一行给值，空分母不能把下限变成 0>=0。
total_numeric = sum(1 for (_, _, e) in progress if e != "n/a")
assert total_numeric > 0, f"{name}: every progress line rendered n/a; C-122 rejects an all-n/a run"

# 判据四（R0，ADR-014）：真值 T 落在该行推出的 [lo,hi] 内时误差必须 <20%；
# T 在区间外时该行只计覆盖率。[lo,hi] 一律从日志按 C-122 区间口径重算，
# 不读实现内部。
end = progress[-1][0]
emitted = checked = inrange = exempt = no_window = 0
for (t, pct, eta) in progress:
    if eta == "n/a":
        continue
    emitted += 1
    remaining = end - t
    # 整秒 ETA 在不足 3s 的窗口上光渲染精度就超出 20% 预算，故尾巴不算误差证据。
    # 这是测量口径，不是判据。
    if remaining < 3:
        continue
    # 判据与实现同一粒度（C-122 区间口径，ADR-016 证据粒度）：区间取该行窗口内
    # 的 2s 块速率，外加全窗口吞吐（媒体增量 ÷ 墙钟，含等值行占用的时间）作为
    # 慢端下限——等值行不得把速率抬高。
    batch = [i for i, row in enumerate(work) if t - W < row[0] <= t]
    rates = []
    for i in batch:
        r = chunk_rate(i, batch[0])
        if r:
            rates.append(r)
    if len(rates) < 2 or len(batch) < 2:
        # 该行没有可重算的区间（块不足两条）；显式记数，不静默跳过。
        no_window += 1
        continue
    t0, t1 = work[batch[0]][0], work[batch[-1]][0]
    if t1 <= t0:
        no_window += 1
        continue
    win_rate = (work[batch[-1]][1] - work[batch[0]][1]) / 100.0 * media_seconds / (t1 - t0)
    if win_rate <= 0:
        no_window += 1
        continue
    rem = media_rem(work[batch[-1]][1])
    lo = max(rem - LAG, 0.0) / max(rates)
    hi = rem / min(min(rates), win_rate) + FINALIZE
    if remaining < lo or remaining > hi:
        exempt += 1
        continue
    inrange += 1
    parts = [int(x) for x in eta.split(":")]
    seconds = parts[-1] + 60 * parts[-2] + (3600 * parts[0] if len(parts) == 3 else 0)
    error = abs(seconds - remaining) / remaining
    assert error < 0.2, (
        f"{name}: ETA {eta} at {t:.3f}s with {remaining:.3f}s left is {error * 100:.0f}% off "
        f"(inside [{lo:.2f},{hi:.2f}])")
    checked += 1
print(f"R0 no-window {no_window}", file=sys.stderr)
denom = len(counted(work))
seg_txt = []
for seg in segs:
    clipped = counted(seg)
    if len(clipped) < 2:
        continue
    seg_txt.append(f"{sum(1 for (_, _, e) in clipped if e != 'n/a')}/{len(clipped)}")
print(f"METRICS {name} rows={len(work)} numeric={emitted} denom={denom} segs={','.join(seg_txt) or '-'} inrange={inrange} exempt={exempt}", file=sys.stderr)
print(f"{checked} {emitted} {segments} {inrange} {exempt}")
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
	samples="$(check_progress "$log" "$name" "$MEDIA_SECONDS")" || fail "$name: progress/ETA contract violated"
	read -r CHECKED EMITTED SEGMENTS INRANGE EXEMPT <<<"$samples"
	echo "PASS: $name: monotonic 0->100, ETA segment on every line, ${INRANGE} in-range numeric lines < 20% (${EXEMPT} exempt), ${SEGMENTS} steady segments carry at least half numeric (${CHECKED} checked of ${EMITTED} numeric; scope: >= 3s before end)"
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
