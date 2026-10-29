#!/usr/bin/env bash
# TC-security-07: 健康探针应答字段面（AC-49）
#
# 可观察判据（docs/03-acceptance.md AC-49）:
#   - 不携带凭据: 200，键集合等于 {status}
#   - 携带有效凭据: 200，键集合为 {status, version, auth_enabled}，三键恒在
#   - 携带无效凭据: 401，错误体按 C-52，响应不含任何健康字段
#   - Server 未配置令牌时携带凭据: 按无效凭据处理，返回 401
#   - 两条路径（/health 与 /api/v1/health）在键集合、状态码与错误体上逐项同形
#
# 判定按「键是否存在」，不比较键值：允许集合之外的键出现即失败，集合内的
# 键缺失同样失败。

set -euo pipefail
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

PATHS=(/health /api/v1/health)

# probe prints the HTTP status of one health request and writes its body to
# <body-file>. An empty <auth> sends no Authorization header at all, which is
# the only form that counts as "no credential".
probe() {
	local base_url="$1" path="$2" auth="$3" body_file="$4"
	local -a args=(-sS -o "$body_file" -w '%{http_code}')
	[[ -z "$auth" ]] || args+=(-H "Authorization: $auth")
	curl "${args[@]}" "$base_url$path"
}

# keys prints the sorted top-level key set of a JSON object on one line.
keys() {
	python3 -c 'import json,sys; print(" ".join(sorted(json.load(open(sys.argv[1])))))' "$1"
}

# same_json reports whether two files parse to equal JSON values.
same_json() {
	python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])) == json.load(open(sys.argv[2])) else 1)' "$1" "$2"
}

# expect_keys asserts the body carries exactly the given key set.
expect_keys() {
	local body="$1" want="$2" label="$3" got
	got="$(keys "$body")"
	[[ "$got" == "$want" ]] || fail "$label: keys = [$got], want [$want] (body: $(cat "$body"))"
}

# expect_unauthorized asserts the C-52 error body for a rejected credential
# and that no health field leaked into it.
expect_unauthorized() {
	local body="$1" label="$2" code leaked
	code="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("code",""))' "$body")"
	[[ "$code" == "unauthorized" ]] || fail "$label: code = '$code', want unauthorized (body: $(cat "$body"))"
	leaked="$(python3 -c 'import json,sys; print(" ".join(sorted(set(json.load(open(sys.argv[1]))) & {"status","version","auth_enabled"})))' "$body")"
	[[ -z "$leaked" ]] || fail "$label: 401 body carries health fields: $leaked"
}

# run_form probes both paths for one credential form, asserts the expected
# status and body shape on each, then asserts the two paths agree — the
# contract's per-item sameness rule.
run_form() {
	local base_url="$1" label="$2" auth="$3" want_status="$4" want_keys="$5" tag="$6"
	local i code body
	for i in 0 1; do
		body="$TMP_ROOT/$tag-$i.json"
		code="$(probe "$base_url" "${PATHS[$i]}" "$auth" "$body")"
		[[ "$code" == "$want_status" ]] || fail "$label: ${PATHS[$i]} status $code, want $want_status"
		if [[ "$want_status" == "200" ]]; then
			expect_keys "$body" "$want_keys" "$label: ${PATHS[$i]}"
		else
			expect_unauthorized "$body" "$label: ${PATHS[$i]}"
		fi
	done
	[[ "$(keys "$TMP_ROOT/$tag-0.json")" == "$(keys "$TMP_ROOT/$tag-1.json")" ]] ||
		fail "$label: key sets differ between ${PATHS[0]} and ${PATHS[1]}"
	[[ "$want_status" == "200" ]] || same_json "$TMP_ROOT/$tag-0.json" "$TMP_ROOT/$tag-1.json" ||
		fail "$label: error bodies differ between ${PATHS[0]} and ${PATHS[1]}"
}

# free_port prints a port the kernel currently considers free. The probe socket
# closes at once; the caller is its only user. A fixed offset would collide with
# a neighbouring acceptance script, which takes PORT+1, PORT+2, ...
free_port() {
	python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

# start_tokenless_server starts a second server without --auth-token and sets
# NOAUTH_URL. It never serves business traffic; only the fail-closed credential
# branch is exercised against it.
start_tokenless_server() {
	local port data_dir deadline
	port="$(free_port)"
	data_dir="$TMP_ROOT/noauth-data"
	mkdir -p "$data_dir"
	"$RFFMPEG_BIN/rffmpeg-server" --port "$port" --data-dir "$data_dir" \
		>>"$TMP_ROOT/noauth-server.log" 2>&1 &
	echo "$!" >>"$PIDFILE"
	NOAUTH_URL="http://127.0.0.1:$port"
	deadline=$((SECONDS + 30))
	until [[ "$(curl -s -o /dev/null -w '%{http_code}' "$NOAUTH_URL/health" 2>/dev/null)" == "200" ]]; do
		((SECONDS < deadline)) ||
			{ echo "tokenless server did not answer; see $TMP_ROOT/noauth-server.log" >&2; return 1; }
		sleep 0.2
	done
}

build_binaries
start_server
wait_server_ready
URL="$(acceptance_server_url)"

# 1. 不携带凭据：公开面只有 status。
run_form "$URL" "anonymous probe" "" 200 "status" anon

# 2. 携带有效凭据：三个诊断键恒在。
run_form "$URL" "valid credential" "Bearer $AUTH_TOKEN" 200 "auth_enabled status version" authed

# 3. 携带无效凭据：401，错误体按 C-52，不含健康字段。
run_form "$URL" "invalid credential" "Bearer invalid-token" 401 "" invalid

# 4. Server 未配置令牌：携带凭据按无效凭据处理；无凭据探测照常应答最小面。
start_tokenless_server
run_form "$NOAUTH_URL" "tokenless server, credential presented" "Bearer $AUTH_TOKEN" 401 "" tokenless-cred
run_form "$NOAUTH_URL" "tokenless server, anonymous probe" "" 200 "status" tokenless-anon

echo "PASS: AC-49 field surface holds on ${PATHS[0]} and ${PATHS[1]} (anonymous/valid/invalid/tokenless)"
