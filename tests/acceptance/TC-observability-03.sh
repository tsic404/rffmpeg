#!/usr/bin/env bash
# TC-observability-03: 进度单调与 ETA 段（AC-40）
#
# 判据：百分比单调 0%→100%；含 "Progress:" 的行都必须带 ETA 段（n/a 或时长格式）；
# 带数值行的误差 <20%（真值不在该行推出的区间内者只计覆盖率）；
# 稳定段内带数值行不少于该段一半；无稳定段时改判运行级下限 15%（分母为空按 0 行计）。
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
import re
import sys
from decimal import Decimal

name, media_seconds = sys.argv[2], float(sys.argv[3])
# 进度行按 C-122 保留精确网格：时刻取整到毫秒、百分比取整到 0.1 个百分点的
# 整数十分位。块区间的端点是 (dp±1)/span，两个端点是否相接因此由整数比较
# 决定，double 的 4e-15 误差不能把「相接」读成「分离」。
line_re = re.compile(r"^([0-9.]+)\tProgress: ([0-9.]+)% \| ETA: (n/a|[0-9]+:[0-9]{2}(?::[0-9]{2})?)$")
progress = []  # (t_ms, pct_tenths, eta)
for line in open(sys.argv[1]):
	row = line.rstrip("\n")
	if "Progress:" not in row:
		continue
	# 判据二：进度行必须带 ETA 段（n/a 或时长格式），不合形的行不再静默丢弃。
	m = line_re.match(row)
	assert m, f"{name}: progress line without a well-formed ETA segment: {row!r}"
	t_ms = int((Decimal(m.group(1)) * 1000).to_integral_value())
	pct = int((Decimal(m.group(2)) * 10).to_integral_value())
	progress.append((t_ms, pct, m.group(3)))

assert progress, f"{name}: no Progress lines"
pcts = [p for _, p, _ in progress]
assert all(a <= b for a, b in zip(pcts, pcts[1:])), f"{name}: progress is not monotonic"
assert progress[-1][1] == 1000, f"{name}: last progress line is {progress[-1][1]/10}%, want 100%"

BAND = 0.15    # 累计带宽（C-122）
W_MS = 10000   # 证据窗口（采样/管道属性）
LAG = 5.0      # 输出延迟（媒体秒）
FINALIZE = 0.75  # 收尾界（墙钟秒）
START_UP_MS = 18000  # 管道实测起步时长上界（墙钟毫秒）
# 块跨度包络从 C-122 推：块长 2s；块首行取距起点最近的一行，「可落在起点
# 之前一个采样间隔内」。采样间隔是 C-102 的推送节流（≥1s），首行最多落在
# 起点前 1s，故跨度最多 2s + 1s = 3s。超过它的块是客户端成批刷出后
# 「1ms 相邻对 + 3.9s 洞」的产物：首行落在起点前 1.856s，实现按推送时刻
# 取样造不出这个块，判据也不能拿它算速率。
CHUNK_MS = 2000
SAMPLE_MS = 1000
BLOCK_MIN_MS = CHUNK_MS // 2
BLOCK_MAX_MS = CHUNK_MS + SAMPLE_MS
# 两条渲染行相差不足半个采样间隔时，不可能是两次独立推送的渲染时刻：C-102
# 的推送节流是 1s，本机实测渲染抖动最多 310ms（0.69s 间隔的渲染对），再留
# 同等余量，半个间隔（500ms）是能同时容纳抖动与「一次刷出」的分界。更密的
# 相邻对只能是 CLI 把已排队的帧一次刷出（C-103），其时间戳是刷出时刻。
FLUSH_GAP_MS = SAMPLE_MS // 2

# 判据与实现同一份块序列（C-122 区间口径）：100% 行保留在序列里（它只落在
# 收尾证据窗口内，不进入任何有数值行的相邻对）。块以该行为末行、取距起点
# （末行 − 2s）最近的一行为首行；首块在窗口起点截短。
#
# 渲染时间戳不是推送时刻：CLI 把已排队的进度成批刷出（C-103）时，同一批
# 行的渲染时刻相同或只差几毫秒，先出的那些行的时间戳无法定位它们对应的
# 采样点（C-102 的推送节流保证相邻推送 ≥1s，见 FLUSH_GAP_MS 的推导）。
# 这些行仍是进度行（计入单调与覆盖率分母），但不能充当块的末行或首行；
# 块的进度差从上一个可用采样点算到下一个可用末行，被跳过的百分比增量仍
# 计入，不蒸发。
work = progress
rows_t = [r[0] for r in work]
unusable = [i + 1 < len(work) and rows_t[i + 1] - rows_t[i] < FLUSH_GAP_MS
	for i in range(len(work))]

def block_at(i, base):
	# 返回 (dp_tenths, span_ms)；不产生块速率时为 None：
	# 不足两行、没有进度差、首末墙钟差 <1s，或首行落到起点前超过一个采样
	# 间隔（跨度 > 2s + 1s）时该块不产生速率（C-122）。
	if i <= base or unusable[i]:
		return None
	start = rows_t[i] - CHUNK_MS
	candidates = [k for k in range(base, i) if not unusable[k]]
	if not candidates:
		return None
	j = min(candidates, key=lambda k: abs(rows_t[k] - start))
	span = rows_t[i] - rows_t[j]
	dp = work[i][1] - work[j][1]
	if span < BLOCK_MIN_MS or span > BLOCK_MAX_MS or dp <= 0:
		return None
	return (dp, span)

def block_rate(b):
	dp, span = b
	return dp / 1000.0 * media_seconds / (span / 1000.0)

def pace_step(prev, cur):
	# C-122 阶跃款：两块速率区间（闭区间）严格分离，且点估计的速率比仍
	# 超出 0.25。区间端点 = (dp±1)/span，整数交叉相乘即精确比较。
	pdp, pspan = prev
	cdp, cspan = cur
	disjoint = (pdp + 1) * cspan < (cdp - 1) * pspan or (cdp + 1) * pspan < (pdp - 1) * cspan
	if not disjoint:
		return False
	lhs = 4 * cdp * pspan
	rhs = 5 * pdp * cspan
	return lhs > rhs or lhs * 5 < rhs * 3

blocks = [block_at(i, 0) for i in range(len(work))]

# 稳定段：阶跃或带宽越界切段。零速率块不参与取极值、也不切断序列（C-122），
# 因此与上一块比较的是最近一个速率块，而不是前一行。
segs, seg_start = [], 0
chunk_breaks, chunk_steps = [], []
last_block = None
for i in range(1, len(work)):
	b = blocks[i]
	step = bool(last_block is not None and b is not None and pace_step(last_block, b))
	trs = [block_rate(blocks[j]) for j in range(seg_start + 2, i + 1) if blocks[j]]
	band = len(trs) >= 2 and max(trs) / min(trs) - 1 > BAND
	if step:
		chunk_steps.append(rows_t[i])
	if step or band:
		segs.append((seg_start, i - 1))
		seg_start = i
		chunk_breaks.append(rows_t[i])
	if b is not None:
		last_block = b
segs.append((seg_start, len(work) - 1))
segs = [seg for seg in segs if seg[1] - seg[0] + 1 >= 2]

# 起步段边界：进程启动到速率在起步之后重新稳定为止。首次速率阶跃落在管道
# 起步上界内时以它为界；没有跃迁的运行以管道实测起步时长为上界。
first = work[0][0]
startup_end = first + START_UP_MS
for b in chunk_steps:
	if b - first <= START_UP_MS:
		startup_end = b
	break
last = progress[-1][0]
excluded = [(0, startup_end + W_MS)]
excluded += [(b, b + W_MS) for b in chunk_breaks]
excluded.append((last - W_MS, 10 ** 18))

def counted(rows):
	return [row for row in rows if not any(a < row[0] <= b for a, b in excluded)]

# 稳定段至少含两个相邻速率块（C-122 定义款）：只由零速率块组成的区间不是
# 稳定段，单块也不构成稳定段。段是极大连续区间，段内不会出现阶跃，故段内
# 速率块数 ≥2 即等价于「至少两个相邻速率块」。
def seg_rate_blocks(seg):
	return [blocks[k] for k in range(seg[0], seg[1] + 1) if blocks[k] is not None]

# 判据三：有稳定段时段内带数值行 ≥ 一半；否则改判运行级下限 15%。
segments = 0
for seg in segs:
	clipped = counted([work[k] for k in range(seg[0], seg[1] + 1)])
	if len(clipped) < 2 or len(seg_rate_blocks(seg)) < 2:
		continue
	segments += 1
	numeric = sum(1 for (_, _, e) in clipped if e != "n/a")
	assert numeric * 2 >= len(clipped), (
		f"{name}: steady segment [{clipped[0][0]/1000:.1f}s, {clipped[-1][0]/1000:.1f}s] carries "
		f"{numeric}/{len(clipped)} numeric ETA lines; C-122 requires at least half")
# 判据二：阶跃行不给值（C-122）。阶跃判在该行自己的 10s 窗口上：推得该行
# 上下界的那批块由窗口内的行给出，窗口起点所在的首块截短到窗口起点。
# 零速率块不隔断相邻：比较的是窗口内相邻的两个速率块。
def window_blocks(t):
	# 左缘闭合（t-W <= row），与实现 etaRateRange 的 now.Sub(...) <= 10s 同向：
	# 恰好落在 10s 边界上的行两侧都算窗口内，块集合与首块截短起点一致。
	batch = [i for i in range(len(work)) if t - W_MS <= rows_t[i] <= t]
	seq = []
	for i in batch:
		b = block_at(i, batch[0])
		if b is not None:
			seq.append(b)
	return seq

def window_step(t):
	last_block = None
	for b in window_blocks(t):
		if last_block is not None and pace_step(last_block, b):
			return True
		last_block = b
	return False

crit2_judged = 0
transition_rows = []
noblock_rows = []
for (t, pct, eta) in progress:
	if eta == "n/a" or t <= startup_end:
		continue
	if len(window_blocks(t)) < 2:
		noblock_rows.append((t, eta))
		continue
	crit2_judged += 1
	if window_step(t):
		transition_rows.append((t, eta))
print(f"{name}: criterion 2 judged {crit2_judged} rows "
	f"({len(noblock_rows)} with fewer than two measurable blocks)", file=sys.stderr)
for (t, eta) in transition_rows:
	print(f"{name}: ETA {eta} at {t/1000:.3f}s while a step shares its window", file=sys.stderr)
for (t, eta) in noblock_rows:
	print(f"{name}: ETA {eta} at {t/1000:.3f}s in a window with fewer than two measurable rate blocks",
		file=sys.stderr)
# 窗口内不足两个速率块时实现必须渲染 n/a（C-122 门槛款）；给了秒数就是违约。
# 「判不了」不能呈现为通过，所以这些行计入失败，而不是跳过。
assert not noblock_rows, (
	f"{name}: {len(noblock_rows)} numeric ETA line(s) have fewer than two measurable "
	f"rate blocks in their 10s window; C-122 requires n/a there")
assert crit2_judged > 0, (
	f"{name}: criterion 2 judged no row ({len(noblock_rows)} numeric row(s) had fewer than two "
	f"measurable blocks); a log this sparse cannot prove the step clause")
assert not transition_rows, f"{name}: {len(transition_rows)} numeric ETA lines on transition windows"
if segments == 0:
	floor = counted(work)
	numeric = sum(1 for (_, _, e) in floor if e != "n/a")
	# 分母为空（全部进度行落在排除窗口内）时下限按 0 行计，不要求分母非空。
	assert numeric * 100 >= 15 * len(floor), (
		f"{name}: no steady segment and only {numeric}/{len(floor)} numeric ETA lines; "
		f"C-122 run-level floor is 15% of the excluded-window denominator")
# 全 n/a 的运行不通过：空分母下下限是 0 行，这是该情形唯一还有增量的判据。
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
	remaining = (end - t) / 1000.0
	# 整秒 ETA 在不足 3s 的窗口上光渲染精度就超出 20% 预算，故尾巴不算误差证据。
	if remaining < 3:
		continue
	batch = [i for i in range(len(work)) if t - W_MS <= rows_t[i] <= t]
	rates = []
	for i in batch:
		b = block_at(i, batch[0])
		if b:
			rates.append(block_rate(b))
	if len(rates) < 2 or len(batch) < 2:
		no_window += 1
		continue
	t0, t1 = rows_t[batch[0]], rows_t[batch[-1]]
	if t1 <= t0:
		no_window += 1
		continue
	win_rate = (work[batch[-1]][1] - work[batch[0]][1]) / 1000.0 * media_seconds / ((t1 - t0) / 1000.0)
	if win_rate <= 0:
		no_window += 1
		continue
	rem = (100.0 - work[batch[-1]][1] / 10.0) / 100.0 * media_seconds
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
		f"{name}: ETA {eta} at {t/1000:.3f}s with {remaining:.3f}s left is {error * 100:.0f}% off "
		f"(inside [{lo:.2f},{hi:.2f}])")
	checked += 1
print(f"R0 no-window {no_window}", file=sys.stderr)
denom = len(counted(work))
seg_txt = []
for seg in segs:
	clipped = counted([work[k] for k in range(seg[0], seg[1] + 1)])
	if len(clipped) < 2 or len(seg_rate_blocks(seg)) < 2:
		continue
	seg_txt.append(f"{sum(1 for (_, _, e) in clipped if e != 'n/a')}/{len(clipped)}")
denom_numeric = sum(1 for (_, _, e) in counted(work) if e != "n/a")
print(f"METRICS {name} rows={len(work)} numeric={emitted} denom={denom} denom_numeric={denom_numeric} "
	f"segs={','.join(seg_txt) or '-'} steps={len(chunk_steps)} "
	f"transition_rows={len(transition_rows)} inrange={inrange} exempt={exempt} "
	f"crit2_judged={crit2_judged} crit2_noblocks={len(noblock_rows)}", file=sys.stderr)
print(f"{checked} {emitted} {segments} {inrange} {exempt}")
PY
}

# check_capture <name> <log> [media-seconds]: judge a captured CLI log with the
# same check_progress as the scripted cases.
check_capture() {
	local name="$1" log="$2" media="${3:-$MEDIA_SECONDS}" samples
	samples="$(check_progress "$log" "$name" "$media")" || fail "$name: progress/ETA contract violated"
	read -r CHECKED EMITTED SEGMENTS INRANGE EXEMPT <<<"$samples"
	echo "PASS: $name (captured log): ${INRANGE} in-range numeric lines < 20% (${EXEMPT} exempt), ${SEGMENTS} steady segments carry at least half numeric (${CHECKED} checked of ${EMITTED} numeric; scope: >= 3s before end)"
}

# expect_check_fail <name> <log> <expected-message> [media-seconds]: the same
# check_progress must reject the log with the named assertion. Used for
# synthetic fixtures that encode a contract violation, so a passing fixture
# cannot be mistaken for a pass and a rejection for the wrong reason is caught.
expect_check_fail() {
	local name="$1" log="$2" want="$3" media="${4:-$MEDIA_SECONDS}"
	if check_progress "$log" "$name" "$media" >/dev/null 2>"$TMP_ROOT/$name.err"; then
		fail "$name: checker accepted a log that violates the ETA contract"
	fi
	grep -q "$want" "$TMP_ROOT/$name.err" ||
		fail "$name: checker rejected the log for the wrong reason (want: $want)"
	echo "PASS: $name: checker rejects the synthetic violation"
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

# 判据回归夹具：客户端成批刷出（1ms 相邻对 + 3.9s 洞）不得被判成阶跃；
# 空窗口给值、阶跃窗口给值都必须判红。
check_capture tc03-client-jitter "$ENV_DIR/testdata/tc03-client-jitter.log"
expect_check_fail tc03-empty-window "$ENV_DIR/testdata/tc03-empty-window.log" \
	"fewer than two measurable rate blocks"
expect_check_fail tc03-step-numeric "$ENV_DIR/testdata/tc03-step-numeric.log" \
	"numeric ETA lines on transition windows"

run_drift drift-warmup warmup
run_drift drift-dip dip
run_drift drift-burst burst

# TC_EXTRA_LOGS: optional space-separated "<name>=<path>" entries. Each captured
# CLI log is judged by the same check_progress as the scripted cases, against
# MEDIA_SECONDS (every capture here is a 300s window). This is how a real-source
# capture is re-checked without re-running it: the criteria stay single-sourced.
if [[ -n "${TC_EXTRA_LOGS:-}" ]]; then
	for entry in $TC_EXTRA_LOGS; do
		name="${entry%%=*}"
		log="${entry#*=}"
		[[ -f "$log" ]] || fail "TC_EXTRA_LOGS: $log not found"
		check_capture "$name" "$log"
	done
fi
