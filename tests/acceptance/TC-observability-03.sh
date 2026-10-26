#!/usr/bin/env bash
# TC-observability-03: 进度单调与 ETA 段（AC-40）
# 判据：百分比单调 0%→100%；含 "Progress:" 的行都必须带 ETA 段（n/a 或时长格式）；
# ETA 误差 < 20%，只覆盖距结束 ≥3s 的数值 ETA。
# 数值 ETA 出现频次不是判据：契约允许每行 `n/a`。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media eta.mp4 120 1280x720)"
LOG="$TMP_ROOT/stamped.log"
python3 - "$LOG" "$RFFMPEG_BIN/rffmpeg" \
	--server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -preset veryslow "$TMP_ROOT/eta-out.mp4" <<'PY'
import subprocess, sys, time

log = open(sys.argv[1], "w")
proc = subprocess.Popen(sys.argv[2:], stderr=subprocess.PIPE, text=True, bufsize=1)
start = time.monotonic()
for line in proc.stderr:
    print(f"{time.monotonic() - start:.3f}\t{line.rstrip()}", file=log, flush=True)
log.close()
sys.exit(proc.wait())
PY

SAMPLES="$(python3 - "$LOG" <<'PY'
import re, sys

progress = []
line_re = re.compile(r"^([0-9.]+)\tProgress: ([0-9.]+)% \| ETA: (n/a|[0-9]+:[0-9]{2}(?::[0-9]{2})?)$")
for line in open(sys.argv[1]):
    row = line.rstrip("\n")
    if "Progress:" not in row:
        continue
    # 判据二：进度行必须带 ETA 段（n/a 或时长格式），不合形的行不再静默丢弃。
    m = line_re.match(row)
    assert m, f"progress line without a well-formed ETA segment: {row!r}"
    progress.append((float(m.group(1)), float(m.group(2)), m.group(3)))

assert progress, "no Progress lines"
pcts = [p for _, p, _ in progress]
assert all(a <= b for a, b in zip(pcts, pcts[1:])), "progress is not monotonic"
assert progress[-1][1] == 100.0, progress[-1]

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
    assert error < 0.2, (t, pct, eta, remaining, error)
    checked += 1
# checked/emitted 随宿主编码速度与估算器自身的可证性波动，只作为观测值输出。
print(f"{checked} {emitted}")
PY
)" || fail "progress/ETA contract violated"

read -r CHECKED EMITTED <<<"$SAMPLES"
echo "PASS: monotonic 0->100, ETA segment on every line, ETA error < 20% (verified on ${CHECKED} of ${EMITTED} numeric ETAs; scope: >= 3s before end)"
