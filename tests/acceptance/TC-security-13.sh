#!/usr/bin/env bash
# TC-security-13: 远程拉取的目标限制（AC-59）
#
# 可观察判据（docs/03-acceptance.md AC-59）:
#   - 远程输入指向回环、链路本地、共享地址空间、未指定（0.0.0.0 / ::）、组播广播
#     或保留段 → 作业失败 INPUT_UNREACHABLE，该目标未收到请求。
#   - 字面地址按自身判定，主机名按解析结果判定，重定向的每一跳同样判定。
#   - 远程输入指向私有地址（RFC1918）的可达媒体源 → 正常拉取，作业完成。
#   - 不在拒绝类别中的目标一律放行：验收环境不保证可达公共地址，该支由实现侧对
#     地址分类器的断言覆盖（pkg/worker TestDeniedRemoteClass / TestAllowedRemoteClasses）。

set -euo pipefail
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media remote-target.mp4 1)"

# 私有地址文件服务器：放行一侧的可达媒体源。
start_http_fileserver "$MEDIA_DIR"
PRIVATE_BASE="$(media_url_base)"

# 回环监听端：任何发往回环的请求都会写进 TRAP_LOG。它是「该目标未收到请求」的
# 取证点——不得出现 GET 行。
TRAP_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
TRAP_LOG="$TMP_ROOT/trap.log"
: >"$TRAP_LOG"
python3 -m http.server "$TRAP_PORT" --bind 127.0.0.1 --directory "$MEDIA_DIR" >>"$TRAP_LOG" 2>&1 &
TRAP_PID=$!
echo "$TRAP_PID" >>"$PIDFILE"
deadline=$((SECONDS + 15))
until curl -fsS -o /dev/null "http://127.0.0.1:$TRAP_PORT/" 2>/dev/null; do
	((SECONDS < deadline)) || fail "loopback trap did not start; see $TRAP_LOG"
	sleep 0.2
done
# 就绪探测本身会留下 GET 行；从这里开始只认新增请求。
TRAP_BASELINE="$(grep -c 'GET ' "$TRAP_LOG" || true)"

# 重定向服务器：私有地址上的一跳，Location 指向回环监听端。
REDIRECT_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
REDIRECT_LOG="$TMP_ROOT/redirect.log"
python3 - "$REDIRECT_PORT" "http://127.0.0.1:$TRAP_PORT/hop.mp4" >>"$REDIRECT_LOG" 2>&1 <<'PY' &
import http.server, sys
port, target = int(sys.argv[1]), sys.argv[2]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(302)
        self.send_header("Location", target)
        self.end_headers()
    def log_message(self, fmt, *args):
        sys.stderr.write("redirect " + fmt % args + "\n")

http.server.HTTPServer(("0.0.0.0", port), Handler).serve_forever()
PY
REDIRECT_PID=$!
echo "$REDIRECT_PID" >>"$PIDFILE"

# run_cli submits one remote input and returns the CLI exit code, leaving its
# stderr in $CLI_LOG.
run_cli() {
	local url="$1"
	CLI_LOG="$TMP_ROOT/cli-$(printf '%s' "$url" | sha256sum | cut -c1-8).log"
	set +e
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$url" -c:v libx264 "$TMP_ROOT/out.mp4" 2>"$CLI_LOG"
	local rc=$?
	set -e
	return $rc
}

# expect_unreachable asserts INPUT_UNREACHABLE with a retryable=false record.
expect_unreachable() {
	local label="$1" url="$2" rc job
	rc=0
	run_cli "$url" || rc=$?
	[[ $rc -eq 1 ]] || fail "$label: rc=$rc, want 1 (see $CLI_LOG)"
	grep -qF 'Job failed: [INPUT_UNREACHABLE]' "$CLI_LOG" ||
		fail "$label: missing INPUT_UNREACHABLE terminal line (see $CLI_LOG)"
	job="$(job_id_from_log "$CLI_LOG")"
	[[ -n "$job" ]] || fail "$label: no job id in CLI log"
	api_get "/api/v1/jobs/$job" | python3 -c '
import json, sys
job = json.load(sys.stdin)["job"]
assert job["failure_type"] == "INPUT_UNREACHABLE", job["failure_type"]
assert job["retryable"] is False, job["retryable"]
' || fail "$label: job record is not INPUT_UNREACHABLE/retryable=false"
}

# 回环（字面地址）。
expect_unreachable "loopback literal" "http://127.0.0.1:$TRAP_PORT/x.mp4"
# 主机名解析到回环。
expect_unreachable "hostname resolving to loopback" "http://localhost:$TRAP_PORT/y.mp4"
# 未指定地址：0.0.0.0 在本机会落到回环监听端，证明策略先于连接生效。
expect_unreachable "unspecified address" "http://0.0.0.0:$TRAP_PORT/z.mp4"
# 链路本地。
expect_unreachable "link-local address" "http://169.254.169.254/latest/meta-data"
# 共享地址空间。
expect_unreachable "shared address space" "http://100.64.0.1/media.mp4"
# 组播与保留段。
expect_unreachable "multicast address" "http://224.0.0.1:9/media.mp4"
expect_unreachable "reserved address" "http://240.0.0.1/media.mp4"
# 重定向的下一跳落到回环：不得跟随到最终目标。
expect_unreachable "redirect hop into loopback" "http://$(private_host_ip):$REDIRECT_PORT/start.mp4"

# 上述被拒目标都没有收到请求：回环监听端与重定向服务器的请求行都不增加。
TRAP_AFTER="$(grep -c 'GET ' "$TRAP_LOG" || true)"
[[ "$TRAP_AFTER" == "$TRAP_BASELINE" ]] ||
	fail "a denied target received a request: loopback trap log grew $TRAP_BASELINE -> $TRAP_AFTER"
grep -q 'redirect ' "$REDIRECT_LOG" || fail "redirect fixture never saw the first hop (the redirect case did not run)"
grep -q 'hop.mp4' "$TRAP_LOG" && fail "the redirect was followed into the loopback target"

# 私有地址（RFC1918）源正常完成。
rc=0
run_cli "$PRIVATE_BASE/$(basename "$MEDIA")" || rc=$?
[[ $rc -eq 0 ]] || fail "private-address source failed (rc=$rc); see $CLI_LOG"
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$TMP_ROOT/out.mp4" >/dev/null ||
	fail "private-address source product is not readable by ffprobe"

echo "PASS: loopback/hostname/unspecified/link-local/shared/multicast/reserved and redirect hops are INPUT_UNREACHABLE with no request received; a private source completes"
