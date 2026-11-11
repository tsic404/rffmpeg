#!/usr/bin/env bash
# TC-security-11: 作业工作区的写入边界（AC-57）
#
# 可观察判据（docs/03-acceptance.md AC-57）:
#   - 以穿越构件构造输入标识与输出名（提交阶段可判者）→ 400 invalid_request。
#   - 该次提交后作业列表无新增行，文件系统上无新建、覆盖或删除，
#     预先放置的哨兵文件内容不变。
#   - 执行阶段失败不构成等价通过：该构造在提交阶段可判，执行侧 INFRA 落点由
#     实现侧断言覆盖（pkg/worker sharedfs 用例）。

set -euo pipefail
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries

MEDIA="$(make_media ws.mp4 1)"
ROOT="$MEDIA_DIR/ws-space"
mkdir -p "$ROOT"
cp "$MEDIA" "$ROOT/input.mp4"
SENTINEL="$ROOT/sentinel.txt"
printf 'sentinel-content\n' >"$SENTINEL"
SENTINEL_SUM="$(sha256sum "$SENTINEL" | awk '{print $1}')"
OUTSIDE="$TMP_ROOT/outside-ws"
mkdir -p "$OUTSIDE"

deploy_with_roots() {
	local roots="$1"
	SERVER_EXTRA_FLAGS="--shared-fs-roots $roots" start_server
	wait_server_api
	start_worker "acceptance-worker-$PORT" RFFMPEG_SHARED_FS_ROOTS="$roots"
	wait_ready
}

job_count() {
	api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("jobs") or []))'
}

submit() {
	local body="$1"
	curl -sS -o "$TMP_ROOT/submit.json" -w '%{http_code}' \
		-H "Authorization: Bearer $AUTH_TOKEN" -H 'Content-Type: application/json' \
		-d "$body" "$(acceptance_server_url)/api/v1/jobs"
}

# snapshot prints "<relative path> <sha256>" for every file under the
# filesystem regions this case must leave untouched.
snapshot() {
	python3 - "$@" <<'PY'
import hashlib, os, sys
for root in sys.argv[1:]:
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for name in sorted(filenames):
            path = os.path.join(dirpath, name)
            try:
                with open(path, "rb") as f:
                    digest = hashlib.sha256(f.read()).hexdigest()
            except OSError as exc:
                digest = "unreadable:%s" % exc
            print(os.path.relpath(path, root), digest)
PY
}

expect_rejected() {
	local label="$1" body="$2" before after code
	before="$(job_count)"
	code="$(submit "$body")"
	[[ "$code" == "400" ]] || fail "$label: status $code, want 400 (body: $(cat "$TMP_ROOT/submit.json"))"
	python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get("code") == "invalid_request" else 1)' \
		"$TMP_ROOT/submit.json" || fail "$label: code is not invalid_request (body: $(cat "$TMP_ROOT/submit.json"))"
	after="$(job_count)"
	[[ "$after" == "$before" ]] || fail "$label: job list grew $before -> $after"
}

deploy_with_roots "$ROOT"
BEFORE="$(snapshot "$ROOT" "$OUTSIDE")"

# 穿越构件的输入标识：绝对路径里嵌 ".."。
expect_rejected "input identifier with an upward segment" \
	"{\"input_files\":[\"$ROOT/x/../../../etc/passwd\"],\"direct_path\":[\"$ROOT/x/../../../etc/passwd\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$ROOT/out.mp4\"}"
# 输出名带穿越构件（非直通模式）。
expect_rejected "output name with an upward segment" \
	"{\"input_files\":[\"http://files.example.test/in.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"../../etc/cron.d/x\"}"
# 直通目标带穿越构件。
expect_rejected "pass-through target with an upward segment" \
	"{\"input_files\":[\"$ROOT/input.mp4\"],\"direct_path\":[\"$ROOT/input.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$ROOT/a/../../escape.mp4\"}"
# 直通输入字段带穿越构件。
expect_rejected "pass-through input field with an upward segment" \
	"{\"input_files\":[\"$ROOT/a/../../etc/passwd\"],\"direct_path\":[\"$ROOT/a/../../etc/passwd\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$ROOT/out.mp4\"}"
# 参数里的本机路径（工作区之外）。
expect_rejected "argument naming a host path outside the manifest" \
	"{\"input_files\":[\"http://files.example.test/in.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-filter_complex\",\"movie=/etc/passwd\",\"-c:v\",\"libx264\"],\"output_filename\":\"out.mp4\"}"

AFTER="$(snapshot "$ROOT" "$OUTSIDE")"
[[ "$BEFORE" == "$AFTER" ]] || {
	diff <(printf '%s\n' "$BEFORE") <(printf '%s\n' "$AFTER") >&2
	fail "rejected submissions changed the filesystem"
}
[[ "$(sha256sum "$SENTINEL" | awk '{print $1}')" == "$SENTINEL_SUM" ]] ||
	fail "sentinel content changed"
[[ ! -e /etc/cron.d/x ]] || fail "traversal output reached /etc/cron.d/x"

echo "PASS: traversal constructions are 400 invalid_request with no job row and no filesystem change"
