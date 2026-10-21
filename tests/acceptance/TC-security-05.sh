#!/usr/bin/env bash
# TC-security-05: 超限 + 重试开关（AC-30）
# 判据：13 个并发提交最终全部 rc=0，重投静默进行。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready
start_worker worker-b
wait_worker worker-b

MEDIA="$(make_media retry.mp4 8)"
N=13
RC_FILE="$TMP_ROOT/rc"
: >"$RC_FILE"

CLI_PIDS=()
for i in $(seq 1 "$N"); do
	(
		set +e
		timeout 600 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
			--retry -i "$MEDIA" -c:v libx264 -preset veryslow "$TMP_ROOT/out-$i.mp4" \
			>/dev/null 2>"$TMP_ROOT/cli-$i.log"
		echo "$? $i" >>"$RC_FILE"
	) &
	CLI_PIDS+=($!)
done
for pid in "${CLI_PIDS[@]}"; do wait "$pid" || true; done

[[ "$(wc -l <"$RC_FILE")" -eq "$N" ]] || fail "only $(wc -l <"$RC_FILE")/$N submissions finished"
while read -r rc i; do
	[[ "$rc" == "0" ]] || fail "submission $i ended rc=$rc (retry must make all 13 succeed)"
	ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 \
		"$TMP_ROOT/out-$i.mp4" >/dev/null || fail "submission $i output is not probeable"
	grep -q 'rate_limit_exceeded' "$TMP_ROOT/cli-$i.log" &&
		fail "retry was not silent on submission $i"
done <"$RC_FILE"

JOBS="$(api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))')"
[[ "$JOBS" == "$N" ]] || fail "expected $N jobs on the server, found $JOBS"

echo "PASS: all $N concurrent submissions succeeded silently under --retry"
