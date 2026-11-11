#!/usr/bin/env bash
# TC-security-10: 输出名与直通目标的形态（AC-56）
#
# 可观察判据（docs/03-acceptance.md AC-56）:
#   - 非直通模式：绝对路径、含目录分隔符、含上溯段或空的输出名 → 400
#     invalid_request，作业不创建；合法输出名的产物可 ffprobe。
#   - 直通模式：输出目标须为绝对路径、不含上溯段、按 C-107 落在任一声明根之下；
#     空串、相对路径、含上溯段、根外 → 400 invalid_request，作业不创建，
#     目标路径无任何写入。
#   - 直通路径字段同样须为绝对路径且落在根内；不满足 → 400，零写入。
#   - 启用但未声明空间根的部署上，直通请求 → 400，零写入。
#   - 合法直通目标：产物落在该路径本身；执行期间工作区外除该目标外无变更。
#   - 提交时在根内、执行时解析到根外的目标没有稳定的提交侧判据，由执行侧拒绝
#     （INFRA，零写入），按实现侧断言覆盖（pkg/worker sharedfs 用例）。

set -euo pipefail
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries

MEDIA="$(make_media out-name.mp4 2)"
ROOT="$MEDIA_DIR/space"
mkdir -p "$ROOT"
cp "$MEDIA" "$ROOT/input.mp4"
OUTSIDE_DIR="$TMP_ROOT/outside"
mkdir -p "$OUTSIDE_DIR"

# free_port / deploy mirror TC-security-09: one isolated server+worker per
# declared space, because the space is a deployment-level declaration.
free_port() {
	python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

deploy() {
	local label="$1" roots="$2"
	shift 2
	DEPLOY_URL="http://127.0.0.1:$(free_port)"
	local data_dir="$TMP_ROOT/data-$label" worker_dir="$TMP_ROOT/worker-$label"
	mkdir -p "$data_dir" "$worker_dir"
	local -a server_args=(--port "${DEPLOY_URL##*:}" --data-dir "$data_dir" --auth-token "$AUTH_TOKEN")
	[[ -z "$roots" ]] || server_args+=(--shared-fs-roots "$roots")
	"$RFFMPEG_BIN/rffmpeg-server" "${server_args[@]}" >>"$TMP_ROOT/server-$label.log" 2>&1 &
	echo "$!" >>"$PIDFILE"

	local deadline=$((SECONDS + 30))
	until curl -fsS -o /dev/null "$DEPLOY_URL/health"; do
		((SECONDS < deadline)) || { echo "server $label unhealthy; see $TMP_ROOT/server-$label.log" >&2; return 1; }
		sleep 0.2
	done
	until curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$DEPLOY_URL/api/v1/workers" 2>/dev/null | grep -q '"workers"'; do
		((SECONDS < deadline)) || { echo "server $label API did not answer" >&2; return 1; }
		sleep 0.1
	done

	env RFFMPEG_SERVER_URL="$DEPLOY_URL" RFFMPEG_TOKEN="$AUTH_TOKEN" \
		RFFMPEG_WORKER_NAME="worker-$label" RFFMPEG_TEMP_DIR="$worker_dir" \
		RFFMPEG_CACHE_ENABLED=false "$@" \
		"$RFFMPEG_BIN/rffmpeg-worker" >>"$TMP_ROOT/worker-$label.log" 2>&1 &
	local worker_pid=$!
	echo "$worker_pid" >>"$PIDFILE"

	deadline=$((SECONDS + WAIT_READY_BUDGET_S))
	until curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$DEPLOY_URL/api/v1/workers" |
		grep -q '"status":"online"\|"status":"idle"\|"status":"busy"'; do
		kill -0 "$worker_pid" 2>/dev/null || { echo "worker $label exited; see $TMP_ROOT/worker-$label.log" >&2; return 1; }
		((SECONDS < deadline)) || { echo "worker $label did not register" >&2; return 1; }
		sleep 0.2
	done
}

job_count() {
	curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$DEPLOY_URL/api/v1/jobs" |
		python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("jobs") or []))'
}

submit() {
	local body="$1" out
	out="$(curl -sS -o "$TMP_ROOT/submit.json" -w '%{http_code}' \
		-H "Authorization: Bearer $AUTH_TOKEN" -H 'Content-Type: application/json' \
		-d "$body" "$DEPLOY_URL/api/v1/jobs")"
	printf '%s %s' "$out" \
		"$(python3 -c 'import json,sys
try:
    print(json.load(open(sys.argv[1])).get("code",""))
except Exception:
    print("")' "$TMP_ROOT/submit.json")"
}

expect_rejected() {
	local label="$1" body="$2" before after got
	before="$(job_count)"
	got="$(submit "$body")"
	[[ "$got" == "400 invalid_request" ]] || fail "$label: got [$got], want [400 invalid_request]"
	after="$(job_count)"
	[[ "$after" == "$before" ]] || fail "$label: job list grew $before -> $after"
}

# snapshot prints a stable digest of every path under the given directory
# (relative name + content hash), so a stray write or deletion is detectable.
snapshot() {
	python3 - "$1" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
for dirpath, dirnames, filenames in os.walk(root):
    dirnames.sort()
    for name in sorted(filenames):
        path = os.path.join(dirpath, name)
        rel = os.path.relpath(path, root)
        try:
            with open(path, "rb") as f:
                digest = hashlib.sha256(f.read()).hexdigest()
        except OSError as exc:
            digest = "unreadable:%s" % exc
        print(rel, digest)
PY
}

expect_same_fs() {
	local before="$1" after="$2" label="$3"
	[[ "$before" == "$after" ]] || {
		diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") >&2
		fail "$label: filesystem changed"
	}
}


# ---------------------------------------------------------------- deployment A
# 共享文件系统启用并声明了根。
deploy space "$ROOT" RFFMPEG_SHARED_FS_ROOTS="$ROOT"

# 非直通：非法输出名逐条 400，零写入。输入用远程 URL：它在分类上合法，不需要
# 服务端已存有对应文件，因此拒绝原因只会来自输出名。
INPUT_ID="http://files.example.test/in.mp4"
expect_rejected "absolute output name in non pass-through mode" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$ROOT/abs.mp4\"}"
expect_rejected "output name with a directory separator" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"sub/dir.mp4\"}"
expect_rejected "output name with an upward segment" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"../escape.mp4\"}"
expect_rejected "empty output name" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"\"}"
[[ ! -e "$ROOT/abs.mp4" && ! -e "$ROOT/sub" && ! -e "$ROOT/../escape.mp4" ]] ||
	fail "rejected output names left filesystem artifacts"

# 非直通：合法输出名的产物可 ffprobe（CLI 提交的唯一文件名）。
LOG="$TMP_ROOT/cli-name.log"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$DEPLOY_URL" --token "$AUTH_TOKEN" \
	-i "$ROOT/input.mp4" -c:v libx264 "$TMP_ROOT/legal-name.mp4" 2>"$LOG"
RC=$?
set -e
[[ $RC -eq 0 ]] || fail "legal output name run failed (rc=$RC); see $LOG"
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$TMP_ROOT/legal-name.mp4" >/dev/null ||
	fail "legal output name product is not readable by ffprobe"

# 直通：非法目标与非法直通路径字段逐条 400，且目标路径无任何写入。
BEFORE="$(snapshot "$ROOT")"
expect_rejected "empty pass-through target" \
	"{\"input_files\":[\"$ROOT/input.mp4\"],\"direct_path\":[\"$ROOT/input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"\"}"
expect_rejected "relative pass-through target" \
	"{\"input_files\":[\"$ROOT/input.mp4\"],\"direct_path\":[\"$ROOT/input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"rel/out.mp4\"}"
expect_rejected "pass-through target with an upward segment" \
	"{\"input_files\":[\"$ROOT/input.mp4\"],\"direct_path\":[\"$ROOT/input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$ROOT/x/../out.mp4\"}"
expect_rejected "pass-through target outside every root" \
	"{\"input_files\":[\"$ROOT/input.mp4\"],\"direct_path\":[\"$ROOT/input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$OUTSIDE_DIR/out.mp4\"}"
expect_rejected "relative pass-through input field" \
	"{\"input_files\":[\"input.mp4\"],\"direct_path\":[\"input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$ROOT/out.mp4\"}"
expect_rejected "pass-through input field outside every root" \
	"{\"input_files\":[\"$OUTSIDE_DIR/in.mp4\"],\"direct_path\":[\"$OUTSIDE_DIR/in.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$ROOT/out.mp4\"}"
AFTER="$(snapshot "$ROOT")"
expect_same_fs "$BEFORE" "$AFTER" "rejected pass-through submissions"
[[ -z "$(ls -A "$OUTSIDE_DIR")" ]] || fail "rejected pass-through submission wrote into $OUTSIDE_DIR"

# 直通：合法目标落在目标路径本身，工作区外除目标外无变更。
TARGET="$ROOT/published.mp4"
BEFORE="$(snapshot "$ROOT")"
LOG="$TMP_ROOT/cli-direct.log"
set +e
env RFFMPEG_SHARED_FS=1 "$RFFMPEG_BIN/rffmpeg" --server "$DEPLOY_URL" --token "$AUTH_TOKEN" \
	-i "$ROOT/input.mp4" -c:v libx264 "$TARGET" 2>"$LOG"
RC=$?
set -e
[[ $RC -eq 0 ]] || fail "legal pass-through run failed (rc=$RC); see $LOG"
[[ -s "$TARGET" ]] || fail "pass-through product is missing at $TARGET"
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$TARGET" >/dev/null ||
	fail "pass-through product is not readable by ffprobe"
AFTER="$(snapshot "$ROOT")"
expect_same_fs \
	"$(printf '%s\n' "$BEFORE" | grep -vF 'published.mp4' || true)" \
	"$(printf '%s\n' "$AFTER" | grep -vF 'published.mp4' || true)" \
	"pass-through run outside its target"
[[ -z "$(ls -A "$OUTSIDE_DIR")" ]] || fail "pass-through run wrote outside its target: $(ls -A "$OUTSIDE_DIR")"

# ---------------------------------------------------------------- deployment C
# 启用但未声明空间根：直通请求 → 400，零写入。
deploy noroot ""
BEFORE="$(snapshot "$ROOT")"
expect_rejected "pass-through request with no declared root" \
	"{\"input_files\":[\"$ROOT/input.mp4\"],\"direct_path\":[\"$ROOT/input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$ROOT/undeclared.mp4\"}"
AFTER="$(snapshot "$ROOT")"
expect_same_fs "$BEFORE" "$AFTER" "undeclared-root pass-through request"

echo "PASS: illegal output names and pass-through targets are 400 with zero writes; legal names and targets land where named with no collateral change"
