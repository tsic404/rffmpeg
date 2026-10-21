#!/usr/bin/env bash
# TC-security-03: 正确 token（AC-28）
# 判据：正常转码，输出可播放。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media goodauth.mp4 2)"
OUT="$TMP_ROOT/out.mp4"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 "$OUT" >/dev/null 2>"$TMP_ROOT/cli.log"
RC=$?
set -e
expect_rc "$RC" 0 "authenticated transcode rc"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 2.0) <= 0.1 else 1)' "$DUR" ||
	fail "unexpected duration: $DUR"

echo "PASS: rc=0, authenticated transcode, duration=$DUR"
