#!/usr/bin/env bash
# TC-transcode-05: 时间裁剪 -ss / -t（AC-08）
# 判据：输出时长落在 3s ± 0.1s。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media source10.mp4 10)"
OUT="$TMP_ROOT/trimmed.mp4"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-ss 4 -t 3 -i "$MEDIA" -c:v libx264 -preset veryfast "$OUT" \
	2>"$TMP_ROOT/cli.log"
RC=$?
set -e
expect_rc "$RC" 0 "trim transcode rc"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 3.0) <= 0.1 else 1)' "$DUR" ||
	fail "trimmed duration $DUR, want 3.0 +/- 0.1"

echo "PASS: rc=0, -ss/-t produced duration=$DUR"
