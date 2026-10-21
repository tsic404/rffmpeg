#!/usr/bin/env bash
# TC-error-09: 10 路并发提交（AC-19）
# 判据：全部完成或全部报错，无部分丢失。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media concurrent.mp4 2)"
N=10
RC_FILE="$TMP_ROOT/rc"
: >"$RC_FILE"

CLI_PIDS=()
for i in $(seq 1 "$N"); do
	(
		set +e
		"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
			-i "$MEDIA" -c:v libx264 -preset veryfast "$TMP_ROOT/out-$i.mp4" \
			>/dev/null 2>"$TMP_ROOT/cli-$i.log"
		echo "$? $i" >>"$RC_FILE"
	) &
	CLI_PIDS+=($!)
done
for pid in "${CLI_PIDS[@]}"; do wait "$pid" || true; done

[[ "$(wc -l <"$RC_FILE")" -eq "$N" ]] || fail "only $(wc -l <"$RC_FILE")/$N submissions reported"

OK=0
BAD=0
while read -r rc i; do
	if [[ "$rc" == "0" ]]; then
		OK=$((OK + 1))
		ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 \
			"$TMP_ROOT/out-$i.mp4" >/dev/null ||
			fail "job $i reported rc=0 but its output is not probeable"
	else
		BAD=$((BAD + 1))
	fi
done <"$RC_FILE"

if (( OK != 0 && BAD != 0 )); then
	fail "partial success: $OK completed, $BAD failed (AC-19 requires all or none)"
fi

echo "PASS: $N concurrent submissions, $OK completed, $BAD failed"
