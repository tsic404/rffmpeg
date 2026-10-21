#!/usr/bin/env bash
# TC-datachan-01: HTTP 远程输入（AC-32）
# 判据：CLI 无上传进度（输入由 Worker 侧拉取）；输出正确。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media remote.mp4 4)"
start_http_fileserver "$MEDIA_DIR"
URL="http://127.0.0.1:$HTTP_PORT/$(basename "$MEDIA")"
LOG="$TMP_ROOT/cli.log"
OUT="$TMP_ROOT/remote-out.mp4"

set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$URL" -c:v libx264 "$OUT" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "remote-input rc"

grep -qF "Using remote input: $URL" "$LOG" || fail "CLI did not treat the input as a remote URL"
grep -qE '^Uploading |^Uploaded: ' "$LOG" && fail "CLI uploaded a remote URL instead of the Worker fetching it"

JOB="$(job_id_from_log "$LOG")"
api_get "/api/v1/jobs/$JOB" |
	python3 -c '
import json, sys
job = json.load(sys.stdin)["job"]
assert job["input_files"] == ["'"$URL"'"], job["input_files"]
' || fail "job input is not the remote URL"

DUR="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OUT")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 4.0) <= 0.1 else 1)' "$DUR" ||
	fail "unexpected duration: $DUR"

echo "PASS: rc=0, no CLI upload, remote URL fetched by Worker, duration=$DUR"
