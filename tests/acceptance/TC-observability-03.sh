#!/usr/bin/env bash
# TC-observability-03: 进度单调与 ETA 段（AC-40）
# 判据：百分比单调 0%→100%；ETA 段每行恒在且匹配 n/a 或时长格式；
# ETA 误差 < 20%（用行时间戳与实际剩余时间比对）。
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

python3 - "$LOG" <<'PY' || fail "progress/ETA contract violated"
import re, sys

progress = []
for line in open(sys.argv[1]):
    m = re.match(r"^([0-9.]+)\tProgress: ([0-9.]+)% \| ETA: (n/a|[0-9]+:[0-9]{2}(?::[0-9]{2})?)$", line.rstrip("\n"))
    if m:
        progress.append((float(m.group(1)), float(m.group(2)), m.group(3)))

assert progress, "no Progress lines"
pcts = [p for _, p, _ in progress]
assert all(a <= b for a, b in zip(pcts, pcts[1:])), "progress is not monotonic"
assert progress[-1][1] == 100.0, progress[-1]

end = progress[-1][0]
checked = 0
for t, pct, eta in progress:
    if eta == "n/a":
        continue
    parts = [int(x) for x in eta.split(":")]
    seconds = parts[-1] + 60 * parts[-2]
    if len(parts) == 3:
        seconds += 3600 * parts[0]
    remaining = end - t
    if remaining < 3:
        continue
    error = abs(seconds - remaining) / remaining
    assert error < 0.2, (t, pct, eta, remaining, error)
    checked += 1
assert checked >= 3, f"only {checked} numeric-ETA samples; need >= 3"
PY

echo "PASS: monotonic 0->100, ETA segment on every line, ETA error < 20%"
