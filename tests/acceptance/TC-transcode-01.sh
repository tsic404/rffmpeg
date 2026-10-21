#!/usr/bin/env bash
# TC-transcode-01: 单输入基础转码（AC-01、AC-02、AC-03、AC-04）
# 判据：无报错；stderr 进度可解析；输出可 ffprobe；同参数与原生 ffmpeg
# 输出逐字节一致。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media base.mp4 4)"
OUT="$TMP_ROOT/out.mp4"
NATIVE="$TMP_ROOT/native.mp4"
LOG="$TMP_ROOT/cli.log"
ARGS=(-c:v libx264 -crf 23 -preset veryfast -threads 1)

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" "${ARGS[@]}" "$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "transcode rc"

grep -qE '^Progress: [0-9]+\.[0-9]% \| ETA: (n/a|[0-9]+:[0-9]{2}(:[0-9]{2})?)$' "$LOG" ||
	fail "no parsable Progress line in stderr"

[[ -f "$OUT" ]] || fail "output missing"
DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 4.0) <= 0.1 else 1)' "$DUR" ||
	fail "unexpected duration: $DUR"

ffmpeg -loglevel error -y -i "$MEDIA" "${ARGS[@]}" "$NATIVE"
cmp -s "$OUT" "$NATIVE" || fail "output differs from native ffmpeg (AC-04)"

echo "PASS: rc=0, progress parsable, duration=$DUR, output identical to native ffmpeg"
