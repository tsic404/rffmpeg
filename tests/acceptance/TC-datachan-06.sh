#!/usr/bin/env bash
# TC-datachan-06: TTL / LRU 驱逐（AC-37）
# 判据：过期条目被清除；超限条目被清除，缓存体积受上限约束。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server

MEDIA="$(make_media evict.mp4 4)"

# --- TTL: a 10s TTL makes an entry expire between two identical runs.
start_worker ttl-worker RFFMPEG_CACHE_ENABLED=true \
	RFFMPEG_CACHE_DIR="$TMP_ROOT/cache-ttl" RFFMPEG_CACHE_TTL=10s
wait_worker ttl-worker

run_crf() {
	local crf="$1" out="$2" log="$3"
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$MEDIA" -c:v libx264 -crf "$crf" "$out" >/dev/null 2>"$log"
}

run_crf 21 "$TMP_ROOT/t1.mp4" "$TMP_ROOT/t1.log"
run_crf 21 "$TMP_ROOT/t2.mp4" "$TMP_ROOT/t2.log"
grep -qF '[rffmpeg] Cache hit:' "$TMP_ROOT/t2.log" || fail "fresh entry did not hit"
sleep 11
run_crf 21 "$TMP_ROOT/t3.mp4" "$TMP_ROOT/t3.log"
grep -qF '[rffmpeg] Cache hit:' "$TMP_ROOT/t3.log" && fail "expired entry still hit after TTL"

# --- LRU: cap the cache at 1 MiB while each entry is ~10 MiB.
kill "$(worker_pid ttl-worker)" 2>/dev/null || true
sleep 1
LRU_DIR="$TMP_ROOT/cache-lru"
start_worker lru-worker RFFMPEG_CACHE_ENABLED=true \
	RFFMPEG_CACHE_DIR="$LRU_DIR" RFFMPEG_CACHE_MAX_SIZE_MB=1
wait_worker lru-worker

# One lossless 1280x720 entry is several MiB, i.e. far above the 1 MiB cap, so
# "cap + one entry" cannot be satisfied by retaining two entries.
LRU_MEDIA="$(make_media evict-lru.mp4 8 1280x720)"
for run in 1 2 3; do
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$LRU_MEDIA" -c:v libx264 -preset ultrafast -crf 0 \
		-metadata "comment=run-$run" \
		"$TMP_ROOT/lru-$run.mp4" >/dev/null 2>&1
done
cache_bytes() {
	local value
	value="$(du -sb "$LRU_DIR" 2>/dev/null | awk '{print $1}')"
	[[ "$value" =~ ^[0-9]+$ ]] || return 1
	printf '%s' "$value"
}

CAP_BYTES=$((1024 * 1024))
ENTRY_BYTES="$(stat -c %s "$TMP_ROOT/lru-1.mp4" 2>/dev/null || true)"
[[ "$ENTRY_BYTES" =~ ^[0-9]+$ ]] || fail "cannot size a cached entry"
BEFORE="$(cache_bytes)" || fail "cannot measure the cache directory"
(( BEFORE > CAP_BYTES )) ||
	fail "test setup wrote ${BEFORE}B; not over the 1 MiB cap, eviction not exercised"

# LRU runs on a fixed 5-minute tick; poll until the hard cap is enforced. The
# tolerable overshoot is at most one entry, because eviction removes entries
# one at a time until the stored volume fits.
DEADLINE=$((SECONDS + 330))
while :; do
	SIZE="$(cache_bytes)" || fail "cannot measure the cache directory"
	(( SIZE <= CAP_BYTES + ENTRY_BYTES )) && break
	(( SECONDS < DEADLINE )) ||
		fail "cache still ${SIZE}B after 330s (cap 1 MiB + one ${ENTRY_BYTES}B entry)"
	sleep 5
done
grep -qiE 'LRU eviction' "$TMP_ROOT/worker-lru-worker.log" || fail "no LRU eviction logged"

echo "PASS: TTL expired the entry; LRU held the cache at ${SIZE}B (cap 1 MiB + one ${ENTRY_BYTES}B entry)"
