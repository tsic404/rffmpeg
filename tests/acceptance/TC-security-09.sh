#!/usr/bin/env bash
# TC-security-09: 输入标识的形态分类（AC-55）
#
# 可观察判据（docs/03-acceptance.md AC-55）:
#   - 启用并声明了空间根的部署上，上传标识、http(s) 远程 URL、根内绝对路径三种
#     形态各自提交后作业可完成。
#   - 未启用共享文件系统的部署上，绝对路径输入 → 400 invalid_request，无新增行。
#   - 启用但未声明根的部署上，绝对路径输入 → 400 invalid_request，无新增行。
#   - 落在所有声明根之外的绝对路径、不属于任何形态的标识，以及含 URL 样式子串但
#     不构成合法绝对 URL 的标识 → 400 invalid_request，作业不落库，无文件系统变更。
#   - 提交侧与执行侧的归类一致性、执行侧归不出形态的落点无法在 E2E 构造，由实现侧
#     断言覆盖（pkg/pathutil TestClassifyInput、pkg/worker 工作区边界用例）。

set -euo pipefail
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries

MEDIA="$(make_media form.mp4 2)"
SHARED_ROOT="$MEDIA_DIR/form" # 声明根：形态三与直通目标的落点
mkdir -p "$SHARED_ROOT"
cp "$MEDIA" "$SHARED_ROOT/input.mp4"
SENTINEL="$TMP_ROOT/sentinel.txt"
printf 'untouched\n' >"$SENTINEL"

# free_port prints a port the kernel currently considers free.
free_port() {
	python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

# deploy starts one server + one worker pair on its own port and sets
# DEPLOY_URL. Extra arguments are handed to the worker as environment
# assignments, which is where the shared-filesystem space is declared. A
# deployment that shares a declaration passes the same value to the server
# through --shared-fs-roots.
deploy() {
	local label="$1" roots="$2"
	shift 2
	DEPLOY_URL="http://127.0.0.1:$(free_port)"
	local data_dir="$TMP_ROOT/data-$label"
	local worker_dir="$TMP_ROOT/worker-$label"
	mkdir -p "$data_dir" "$worker_dir"
	local -a server_args=(--port "${DEPLOY_URL##*:}" --data-dir "$data_dir" --auth-token "$AUTH_TOKEN")
	[[ -z "$roots" ]] || server_args+=(--shared-fs-roots "$roots")
	"$RFFMPEG_BIN/rffmpeg-server" "${server_args[@]}" >>"$TMP_ROOT/server-$label.log" 2>&1 &
	echo "$!" >>"$PIDFILE"

	local deadline=$((SECONDS + 30))
	until curl -fsS -o /dev/null "$DEPLOY_URL/health"; do
		((SECONDS < deadline)) ||
			{ echo "server $label did not become healthy; see $TMP_ROOT/server-$label.log" >&2; return 1; }
		sleep 0.2
	done
	until curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$DEPLOY_URL/api/v1/workers" 2>/dev/null | grep -q '"workers"'; do
		((SECONDS < deadline)) || { echo "server $label API did not answer" >&2; return 1; }
		sleep 0.1
	done

	env RFFMPEG_SERVER_URL="$DEPLOY_URL" \
		RFFMPEG_TOKEN="$AUTH_TOKEN" \
		RFFMPEG_WORKER_NAME="worker-$label" \
		RFFMPEG_TEMP_DIR="$worker_dir" \
		RFFMPEG_CACHE_ENABLED=false \
		"$@" \
		"$RFFMPEG_BIN/rffmpeg-worker" >>"$TMP_ROOT/worker-$label.log" 2>&1 &
	local worker_pid=$!
	echo "$worker_pid" >>"$PIDFILE"

	deadline=$((SECONDS + WAIT_READY_BUDGET_S))
	until curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$DEPLOY_URL/api/v1/workers" |
		grep -q '"status":"online"\|"status":"idle"\|"status":"busy"'; do
		kill -0 "$worker_pid" 2>/dev/null ||
			{ echo "worker $label exited before registering; see $TMP_ROOT/worker-$label.log" >&2; return 1; }
		((SECONDS < deadline)) ||
			{ echo "worker $label did not register within ${WAIT_READY_BUDGET_S}s" >&2; return 1; }
		sleep 0.2
	done
}

# job_count prints how many job rows the deployment holds.
job_count() {
	curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$DEPLOY_URL/api/v1/jobs" |
		python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("jobs") or []))'
}

# submit posts a job body and prints "<http_code> <error_code>".
submit() {
	local body="$1" code_file="$TMP_ROOT/submit.json" out
	out="$(curl -sS -o "$code_file" -w '%{http_code}' \
		-H "Authorization: Bearer $AUTH_TOKEN" -H 'Content-Type: application/json' \
		-d "$body" "$DEPLOY_URL/api/v1/jobs")"
	printf '%s %s' "$out" \
		"$(python3 -c 'import json,sys
try:
    print(json.load(open(sys.argv[1])).get("code",""))
except Exception:
    print("")' "$code_file")"
}

# expect_rejected asserts the 400 invalid_request envelope and a flat job count.
expect_rejected() {
	local label="$1" body="$2" before after got
	before="$(job_count)"
	got="$(submit "$body")"
	[[ "$got" == "400 invalid_request" ]] || fail "$label: got [$got], want [400 invalid_request]"
	after="$(job_count)"
	[[ "$after" == "$before" ]] || fail "$label: job list grew $before -> $after"
}

# run_cli runs the CLI against the current deployment and returns its exit code.
run_cli() {
	local log="$1"
	shift
	set +e
	"$RFFMPEG_BIN/rffmpeg" --server "$DEPLOY_URL" --token "$AUTH_TOKEN" "$@" 2>"$log"
	local rc=$?
	set -e
	return $rc
}

HEX="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
OUTSIDE="$TMP_ROOT/outside.mp4"

# ---------------------------------------------------------------- deployment A
# 共享文件系统启用并声明了根：三种形态各自完成一次转码。
deploy shared "$SHARED_ROOT" RFFMPEG_SHARED_FS_ROOTS="$SHARED_ROOT"
start_http_fileserver "$SHARED_ROOT"

# 形态一：上传标识。CLI 走上传通道，作业输入必须是服务端签发的十六进制标识。
LOG="$TMP_ROOT/cli-upload.log"
run_cli "$LOG" -i "$MEDIA" -c:v libx264 "$TMP_ROOT/out-upload.mp4" ||
	fail "upload-form transcription failed; see $LOG"
UPLOAD_JOB="$(job_id_from_log "$LOG")"
[[ -n "$UPLOAD_JOB" ]] || fail "upload-form run produced no job id"
curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$DEPLOY_URL/api/v1/jobs/$UPLOAD_JOB" | python3 -c '
import json, re, sys
inputs = json.load(sys.stdin)["job"]["input_files"]
assert inputs, "no inputs recorded"
bad = [i for i in inputs if not re.fullmatch(r"[0-9a-f]{64}", i)]
assert not bad, bad
' || fail "upload-form job input is not a server-issued identifier"
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$TMP_ROOT/out-upload.mp4" >/dev/null ||
	fail "upload-form output is not readable by ffprobe"

# 形态二：远程 URL。URL 必须带主机的私有地址：回环目标按 C-128 被拒。
REMOTE_URL="$(media_url_base)/input.mp4"
LOG="$TMP_ROOT/cli-url.log"
run_cli "$LOG" -i "$REMOTE_URL" -c:v libx264 "$TMP_ROOT/out-url.mp4" ||
	fail "remote-URL transcription failed; see $LOG"
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$TMP_ROOT/out-url.mp4" >/dev/null ||
	fail "remote-URL output is not readable by ffprobe"

# 形态三：根内绝对路径（直通模式，按 C-92 直读）。
PASS_OUT="$SHARED_ROOT/out-passthrough.mp4"
LOG="$TMP_ROOT/cli-path.log"
set +e
env RFFMPEG_SHARED_FS=1 "$RFFMPEG_BIN/rffmpeg" --server "$DEPLOY_URL" --token "$AUTH_TOKEN" \
	-i "$SHARED_ROOT/input.mp4" -c:v libx264 "$PASS_OUT" 2>"$LOG"
RC=$?
set -e
[[ $RC -eq 0 ]] || fail "absolute-path transcription failed (rc=$RC); see $LOG"
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$PASS_OUT" >/dev/null ||
	fail "pass-through output is not readable by ffprobe"

# 非法与歧义标识：不落库、不产生写入。
expect_rejected "unclassified identifier" \
	"{\"input_files\":[\"not-an-identifier\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"out.mp4\"}"
expect_rejected "URL-style substring inside an identifier" \
	"{\"input_files\":[\"${HEX}?a://../../../../etc/cron.d/x\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"out.mp4\"}"
expect_rejected "absolute path outside every declared root" \
	"{\"input_files\":[\"$OUTSIDE\"],\"direct_path\":[\"$OUTSIDE\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$OUTSIDE\"}"
[[ ! -e "$OUTSIDE" ]] || fail "out-of-root submission wrote $OUTSIDE"
expect_in_file "$SENTINEL" "untouched"

# ---------------------------------------------------------------- deployment B
# 共享文件系统未启用：绝对路径不构成任何形态。
deploy plain ""
expect_rejected "absolute path on a deployment without the shared filesystem" \
	"{\"input_files\":[\"$SHARED_ROOT/input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"out.mp4\"}"

# ---------------------------------------------------------------- deployment C
# 启用但未声明空间根：同样 fail-closed。
deploy noroot ""
expect_rejected "pass-through request with no declared root" \
	"{\"input_files\":[\"$SHARED_ROOT/input.mp4\"],\"direct_path\":[\"$SHARED_ROOT/input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$SHARED_ROOT/out-noroot.mp4\"}"
[[ ! -e "$SHARED_ROOT/out-noroot.mp4" ]] || fail "undeclared-root deployment wrote $SHARED_ROOT/out-noroot.mp4"
expect_in_file "$SENTINEL" "untouched"

echo "PASS: three forms complete; unclassified, URL-embedded, out-of-root, non-shared and undeclared-root submissions are 400 with no job row and no write"
