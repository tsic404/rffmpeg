#!/usr/bin/env bash
# TC-security-07: 健康探针应答字段面（AC-49）
#
# 可观察判据（docs/03-acceptance.md AC-49）:
#   - 不携带凭据: 200，键集合等于 {status}
#   - 字段存在、值为空（含仅空白）: 按不携带凭据处理，200，键集合等于 {status}，
#     与字段缺失逐项同形
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
# <body-file>. The credential form alone decides the request construction, so
# the forms stay distinguishable on the wire:
#   absent  — no Authorization field is written
#   empty   — `Authorization;` makes curl write the field with an empty value
#   blank   — raw socket, because curl trims a whitespace-only value
#   valid   — `Bearer $AUTH_TOKEN`
#   invalid — `Bearer invalid-token`
probe() {
	local base_url="$1" path="$2" form="$3" body_file="$4"
	local -a args=(-sS -o "$body_file" -w '%{http_code}')
	case "$form" in
	absent) ;;
	empty) args+=(-H 'Authorization;') ;;
	blank) probe_blank "$base_url" "$path" "$body_file"; return ;;
	valid) args+=(-H "Authorization: Bearer $AUTH_TOKEN") ;;
	invalid) args+=(-H "Authorization: Bearer invalid-token") ;;
	*) fail "unknown credential form: $form" ;;
	esac
	curl "${args[@]}" "$base_url$path"
}

# probe_blank sends the whitespace-only credential form over a raw socket and
# prints the HTTP status. curl trims leading and trailing whitespace from a
# header value, so this form never reaches the wire through it; only a
# hand-written request can carry `Authorization:   ` and let the server judge
# the value.
probe_blank() {
	local base_url="$1" path="$2" body_file="$3"
	python3 - "$base_url" "$path" "$body_file" <<'PY'
import socket, sys
from urllib.parse import urlsplit

base, path, out = sys.argv[1], sys.argv[2], sys.argv[3]
url = urlsplit(base)
host, port = url.hostname, url.port or 80
request = (
    "GET %s HTTP/1.1\r\n"
    "Host: %s:%d\r\n"
    "Authorization:   \r\n"
    "Connection: close\r\n"
    "\r\n" % (path, host, port)
)
data = b""
with socket.create_connection((host, port), timeout=15) as sock:
    sock.sendall(request.encode())
    while True:
        chunk = sock.recv(65536)
        if not chunk:
            break
        data += chunk
head, _, body = data.partition(b"\r\n\r\n")
status = int(head.split(b"\r\n")[0].split()[1])
headers = {}
for line in head.split(b"\r\n")[1:]:
    name, _, value = line.partition(b":")
    headers[name.strip().lower()] = value.strip().lower()
if headers.get(b"transfer-encoding") == b"chunked":
    decoded, rest = b"", body
    while rest:
        size_line, _, rest = rest.partition(b"\r\n")
        size = int(size_line.split(b";")[0], 16)
        if size == 0:
            break
        decoded += rest[:size]
        rest = rest[size + 2:]
    body = decoded
with open(out, "wb") as f:
    f.write(body)
print(status)
PY
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
	local base_url="$1" label="$2" form="$3" want_status="$4" want_keys="$5" tag="$6"
	local i code body
	for i in 0 1; do
		body="$TMP_ROOT/$tag-$i.json"
		code="$(probe "$base_url" "${PATHS[$i]}" "$form" "$body")"
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
run_form "$URL" "anonymous probe" absent 200 "status" anon

# 2. 字段存在、值为空：按不携带凭据处理，与字段缺失同形。
run_form "$URL" "empty credential field" empty 200 "status" empty-field

# 3. 字段存在、值仅空白：curl 会裁剪，只有裸 socket 能把该形态送上线路。
run_form "$URL" "whitespace credential field" blank 200 "status" blank-field

# 4. 携带有效凭据：三个诊断键恒在。
run_form "$URL" "valid credential" valid 200 "auth_enabled status version" authed

# 5. 携带无效凭据：401，错误体按 C-52，不含健康字段。
run_form "$URL" "invalid credential" invalid 401 "" invalid

# 6. Server 未配置令牌：携带凭据按无效凭据处理；空值字段仍按无凭据应答最小面。
start_tokenless_server
run_form "$NOAUTH_URL" "tokenless server, credential presented" valid 401 "" tokenless-cred
run_form "$NOAUTH_URL" "tokenless server, empty credential field" empty 200 "status" tokenless-empty
run_form "$NOAUTH_URL" "tokenless server, whitespace credential field" blank 200 "status" tokenless-blank
run_form "$NOAUTH_URL" "tokenless server, anonymous probe" absent 200 "status" tokenless-anon

echo "PASS: AC-49 field surface holds on ${PATHS[0]} and ${PATHS[1]} (absent/empty/blank/valid/invalid/tokenless)"
