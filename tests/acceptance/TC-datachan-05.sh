#!/usr/bin/env bash
# TC-datachan-05: 缓存命中与参数隔离（AC-36）
# 判据：二次同参数出现 `[rffmpeg] Cache hit` 行；不同参数无命中行。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker cache-worker RFFMPEG_CACHE_ENABLED=true \
	RFFMPEG_CACHE_DIR="$TMP_ROOT/cache"
wait_ready

MEDIA="$(make_media cache.mp4 4)"

LOG1="$TMP_ROOT/first.log"
LOG2="$TMP_ROOT/second.log"
LOG3="$TMP_ROOT/third.log"

"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -crf 21 "$TMP_ROOT/a1.mp4" >/dev/null 2>"$LOG1"
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -crf 21 "$TMP_ROOT/a2.mp4" >/dev/null 2>"$LOG2"
grep -qF '[rffmpeg] Cache hit:' "$LOG2" || fail "second identical submission did not hit the cache"

"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -crf 22 "$TMP_ROOT/b1.mp4" >/dev/null 2>"$LOG3"
grep -qF '[rffmpeg] Cache hit:' "$LOG3" && fail "different parameters (-crf 22) hit the cache"

DUR_21="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$TMP_ROOT/a2.mp4")"
DUR_22="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$TMP_ROOT/b1.mp4")"
python3 -c 'import sys; sys.exit(0 if abs(float(sys.argv[1]) - 4.0) <= 0.1 else 1)' "$DUR_21" ||
	fail "cached output duration $DUR_21"
cmp -s "$TMP_ROOT/a1.mp4" "$TMP_ROOT/a2.mp4" || fail "cache hit returned a different file"

echo "PASS: same params hit, different params miss, cached bytes identical"