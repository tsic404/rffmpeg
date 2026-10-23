#!/usr/bin/env bash
# TC-datachan-06: TTL / LRU 驱逐（AC-37）
# 判据：过期条目被清除；超限条目被清除，缓存体积受上限约束；驱逐到预算即停，最新条目仍可命中。
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

# --- LRU: size the budget so exactly one entry fits and two never do.
kill "$(worker_pid ttl-worker)" 2>/dev/null || true
sleep 1

LRU_DIR="$TMP_ROOT/cache-lru"
LRU_MEDIA="$(make_media evict-lru.mp4 8 1280x720)"

run_lru() {
	local run="$1" out="$2" log="$3"
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$LRU_MEDIA" -c:v libx264 -preset ultrafast -crf 0 \
		-metadata "comment=$run" \
		"$out" >/dev/null 2>"$log"
}

# One lossless 1280x720 entry is several MiB. Measure the first entry under a
# generous cap, then restart with the cap rounded up to whole MiB: one entry
# fits, two do not.
start_worker lru-probe RFFMPEG_CACHE_ENABLED=true \
	RFFMPEG_CACHE_DIR="$LRU_DIR" RFFMPEG_CACHE_MAX_SIZE_MB=1024
wait_worker lru-probe
run_lru run-1 "$TMP_ROOT/lru-1.mp4" "$TMP_ROOT/lru-1.log"
ENTRY_BYTES="$(stat -c %s "$TMP_ROOT/lru-1.mp4" 2>/dev/null || true)"
[[ "$ENTRY_BYTES" =~ ^[0-9]+$ ]] || fail "cannot size a cached entry"
(( ENTRY_BYTES > 1024 * 1024 )) ||
	fail "a cached entry is ${ENTRY_BYTES}B; the LRU budget cannot be sized to hold one entry"
CAP_MB=$(( (ENTRY_BYTES + 1024 * 1024 - 1) / (1024 * 1024) ))

kill "$(worker_pid lru-probe)" 2>/dev/null || true
sleep 1
start_worker lru-worker RFFMPEG_CACHE_ENABLED=true \
	RFFMPEG_CACHE_DIR="$LRU_DIR" RFFMPEG_CACHE_MAX_SIZE_MB="$CAP_MB"
wait_worker lru-worker

# Two more distinct keys: each write puts the cache one entry over budget, so
# a correct pass evicts the oldest entry and leaves the newest one.
for run in 2 3; do
	run_lru "run-$run" "$TMP_ROOT/lru-$run.mp4" "$TMP_ROOT/lru-$run.log"
done
cache_bytes() {
	local value
	value="$(du -sb "$LRU_DIR" 2>/dev/null | awk '{print $1}')"
	[[ "$value" =~ ^[0-9]+$ ]] || return 1
	printf '%s' "$value"
}

CAP_BYTES=$(( CAP_MB * 1024 * 1024 ))
SETTLED_ENTRIES=1
SETTLED_META=1

# disk_entry_counts prints "<entry files> <metadata files>" from one find pass:
# removeEntry deletes the file and its metadata separately, so two separate
# listings could straddle both removals. A non-zero find exit (a file vanished
# mid-traversal) is retried by the caller rather than killing the script.
disk_entry_counts() {
	find "$LRU_DIR" -type f 2>/dev/null | awk '
		/\.tmp$/ { next }
		/\.meta$/ { meta++; next }
		{ entries++ }
		END { printf "%d %d", entries + 0, meta + 0 }'
}

# Enforcement may already have run: a worker that re-checks the budget on write
# evicts before this sample, so an over-cap reading is not a precondition. The
# poll below still fails any implementation that leaves the cache over budget
# within 330s, and the eviction log below still proves the cap was exercised.
# The tolerable overshoot is at most one entry, because eviction removes
# entries one at a time until the stored volume fits.
DEADLINE=$((SECONDS + 330))
while :; do
	SIZE="$(cache_bytes)" || fail "cannot measure the cache directory"
	if ! COUNTS="$(disk_entry_counts)"; then
		(( SECONDS < DEADLINE )) ||
			fail "cannot enumerate the cache directory after 330s"
		sleep 5
		continue
	fi
	read -r ENTRY_FILES META_FILES <<<"$COUNTS"
	if (( ENTRY_FILES > SETTLED_ENTRIES || META_FILES > SETTLED_META )); then
		(( SECONDS < DEADLINE )) ||
			fail "cache still ${SIZE}B after 330s (cap ${CAP_BYTES}B + one ${ENTRY_BYTES}B entry); ${ENTRY_FILES} entry files / ${META_FILES} metadata files left"
		sleep 5
		continue
	fi
	(( SIZE <= CAP_BYTES + ENTRY_BYTES )) && break
	(( SECONDS < DEADLINE )) ||
		fail "cache still ${SIZE}B after 330s (cap ${CAP_BYTES}B + one ${ENTRY_BYTES}B entry)"
	sleep 5
done
grep -qiE 'LRU eviction' "$TMP_ROOT/worker-lru-worker.log" || fail "no LRU eviction logged"

# The settled cache holds one entry; it must be the one written last. A rerun of
# that write hitting shows eviction stopped at the budget instead of removing
# the entry it had just stored.
run_lru run-3 "$TMP_ROOT/lru-3-recheck.mp4" "$TMP_ROOT/lru-3-recheck.log"
grep -qF '[rffmpeg] Cache hit:' "$TMP_ROOT/lru-3-recheck.log" ||
	fail "newest entry did not survive eviction: rerun of the last write missed"

echo "PASS: TTL expired the entry; LRU held the cache at ${SIZE}B (cap ${CAP_BYTES}B) and kept the newest entry"
