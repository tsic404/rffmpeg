#!/usr/bin/env bash
# TC-security-04: 超限默认行为（AC-29、F5 负向）
# 判据：至少一个提交 rc=1 且 stderr 为单行限流行；超限作业不排队不创建。
# 配额由两个长作业预先占满，使 fail-fast 判定与提交时序无关。
set -euo pipefail
PORT="${PORT:-18999}"
SERVER_EXTRA_FLAGS="--max-concurrent-jobs-per-client 2"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media ratelimit.mp4 120 1280x720)"

# Upload the input once (through a normal completed run) and harvest its id.
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v libx264 -preset veryfast "$TMP_ROOT/seed.mp4" \
	>/dev/null 2>"$TMP_ROOT/seed.log"
FILE_ID="$(grep -oE 'Uploaded: .* -> [0-9a-f]+' "$TMP_ROOT/seed.log" | awk '{print $NF}' || true)"
[[ -n "$FILE_ID" ]] || fail "seed run did not expose the uploaded file id"

jobs_count() {
	api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))'
}

# Hold both quota slots with long encodes; they release only at terminal state.
for i in 1 2; do
	curl -fsS -X POST -H "Authorization: Bearer $AUTH_TOKEN" \
		-H 'Content-Type: application/json' \
		-d "{\"input_files\":[\"$FILE_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\",\"-preset\",\"veryslow\",\"-threads\",\"2\",\"-y\"],\"output_filename\":\"hold-$i.mp4\"}" \
		"$(acceptance_server_url)/api/v1/jobs" >/dev/null || fail "failed to occupy quota slot $i"
done
BEFORE="$(jobs_count)"
(( BEFORE >= 2 )) || fail "quota jobs not created ($BEFORE)"

RC_FILE="$TMP_ROOT/rc"
: >"$RC_FILE"
CLI_PIDS=()
for i in 1 2 3 4; do
	(
		set +e
		"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
			-i "$MEDIA" -c:v libx264 -preset veryslow "$TMP_ROOT/out-$i.mp4" \
			>/dev/null 2>"$TMP_ROOT/cli-$i.log"
		echo "$? $i" >>"$RC_FILE"
	) &
	CLI_PIDS+=($!)
done
for pid in "${CLI_PIDS[@]}"; do wait "$pid" || true; done

REJECTED=0
while read -r rc i; do
	[[ "$rc" == "1" ]] || fail "submission $i rc=$rc, want fail-fast rc=1"
	REJECTED=$((REJECTED + 1))
	LINES="$(grep -cE '^Error submitting job: job submission failed \[rate_limit_exceeded\]: rate limit exceeded: [0-9]+/2 concurrent jobs$' "$TMP_ROOT/cli-$i.log" || true)"
	[[ "$LINES" == "1" ]] || fail "submission $i lacks the single exact rate-limit line (matches=$LINES)"
done <"$RC_FILE"
(( REJECTED >= 1 )) || fail "no submission was rate-limited"

AFTER="$(jobs_count)"
[[ "$AFTER" == "$BEFORE" ]] || fail "rejected submissions created jobs ($BEFORE -> $AFTER)"

echo "PASS: $REJECTED submissions rejected with the exact single line, no job created"
