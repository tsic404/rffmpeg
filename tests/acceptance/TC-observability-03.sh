#!/usr/bin/env bash
# TC-observability-03: 进度单调与 ETA 段（AC-40）
# 判据：百分比单调 0%→100%；含 "Progress:" 的行都必须带 ETA 段（n/a 或时长格式）；
# ETA 误差 < 20%，只覆盖距结束 ≥3s 的数值 ETA。
# 数值 ETA 出现频次不是判据：契约允许每行 `n/a`。
#
# 两种参数形态各跑一遍，判据完全相同（裁剪不构成例外）：
#   full    无裁剪。
#   in-trim 输入侧 `-ss/-t` 置于 `-i` 之前：ffmpeg 从段首重新计时，
#           time= 在裁剪窗口内从零走到窗口长度，分母必须是窗口而非全片。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media eta.mp4 120 1280x720)"
# 裁剪形态用更长的源：窗口仍是 120s（与无裁剪用例同等的编码工作量），
# 而全片是它的两倍——分母取错时百分比顶到 50%、ETA 翻倍，判据能看见。
MEDIA_LONG="$(make_media eta-long.mp4 240 1280x720)"

# check_progress <log> <case> validates AC-40 on one run's stamped CLI log.
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
    error = abs(seconds - remaining) / remaining
    assert error < 0.2, f"{name}: ETA {eta} at {t:.3f}s with {remaining:.3f}s left is {error * 100:.0f}% off"
    checked += 1
print(f"{checked} {emitted}")
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
	read -r CHECKED EMITTED <<<"$samples"
	echo "PASS: $name: monotonic 0->100, ETA segment on every line, ETA error < 20% (verified on ${CHECKED} of ${EMITTED} numeric ETAs; scope: >= 3s before end)"
}

run_case full "" -i "$MEDIA" -c:v libx264 -preset veryslow
run_case in-trim 120 -ss 60 -t 120 -i "$MEDIA_LONG" -c:v libx264 -preset veryslow
