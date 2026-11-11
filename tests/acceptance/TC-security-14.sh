#!/usr/bin/env bash
# TC-security-14: 协议地址输出目标的形态与地址限制（AC-60）
#
# 可观察判据（docs/03-acceptance.md AC-60）:
#   - 带主机名的绝对协议地址输出名 → 提交接受、作业创建、作业完成；该作业不产生
#     可下载的产物（输出直接写向该地址）。
#   - 目标的私有地址上有可达接收端 → 接收端收到数据。
#   - 目标的字面地址落在 C-128 的拒绝类别 → 400 invalid_request，作业不创建。
#   - 主机名解析落在拒绝类别 → 作业失败 INFRA，目标端未收到连接（ffmpeg 未启动）。
#   - file scheme 与不带主机名的串不构成协议地址目标 → 400，作业不创建。
#
# 验收边界（与 AC-60 的说明一致）：地址分类器对每个拒绝类别的穷尽判定、缓存跳过
# 网络输出（C-96）、以及直通分支不受影响，由实现侧断言覆盖——
# pkg/worker TestDeniedRemoteClass / TestPlanJobOutputNames /
# TestProcessJobProtocolTargetIsHandedToFFmpeg / TestProcessJobDeniedProtocolTargetFailsBeforeFFmpeg，
# pkg/server/handlers TestSubmitJobOutputFilenameProtocolAddress{Accepted,Denied} 与
# TestSubmitJobDirectPathProtocolAddressRejected（直通分支不受影响）。本用例在端到端
# 上取「私有地址推流收到数据」「拒绝类字面地址 400 且无作业行」「主机名解析到回环 →
# INFRA 且接收端零字节」三支。

set -euo pipefail
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media net-output.mp4 2)"
PRIVATE_IP="$(private_host_ip)" || fail "no private (RFC1918) host address to push to"

job_count() {
	api_get /api/v1/jobs |
		python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("jobs") or []))'
}

# start_udp_sink <bind-host> <portfile> <payload> runs a UDP receiver until the
# run is torn down and records every datagram it receives in <payload>. ffmpeg's
# mpegts muxer performs no handshake, so a received byte proves the target really
# was written, and an empty loopback-side sink proves it never was connected.
start_udp_sink() {
	local bind_host="$1" portfile="$2" payload="$3"
	python3 - "$bind_host" "$portfile" "$payload" <<'PY' &
import socket, sys

host, portfile, payload = sys.argv[1], sys.argv[2], sys.argv[3]
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.bind((host, 0))
sock.settimeout(0.2)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))
with open(payload, "ab", buffering=0) as sink:
    while True:
        try:
            data, _ = sock.recvfrom(65536)
        except socket.timeout:
            continue
        sink.write(data)
PY
	echo "$!" >>"$PIDFILE"
}

wait_portfile() {
	local portfile="$1" deadline=$((SECONDS + 10))
	until [[ -s "$portfile" ]]; do
		((SECONDS < deadline)) || fail "UDP sink did not start: $portfile"
		sleep 0.1
	done
}

# run_cli submits one job through the CLI and leaves its stderr in $CLI_LOG.
run_cli() {
	local target="$1"
	CLI_LOG="$TMP_ROOT/cli-$(printf '%s' "$target" | sha256sum | cut -c1-8).log"
	set +e
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$MEDIA" -c:v libx264 -preset ultrafast -t 1 -f mpegts "$target" 2>"$CLI_LOG"
	local rc=$?
	set -e
	return $rc
}

# ---------------------------------------------------------------- accepted
SINK_PORTFILE="$TMP_ROOT/sink.port"
SINK_PAYLOAD="$TMP_ROOT/sink.bin"
: >"$SINK_PAYLOAD"
start_udp_sink "0.0.0.0" "$SINK_PORTFILE" "$SINK_PAYLOAD"
wait_portfile "$SINK_PORTFILE"
SINK_PORT="$(cat "$SINK_PORTFILE")"
TARGET="udp://$PRIVATE_IP:$SINK_PORT/stream"

rc=0
run_cli "$TARGET" || rc=$?
[[ $rc -eq 0 ]] || fail "private-address push target failed (rc=$rc); see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "private-address push target: no job id in the CLI log"
[[ "$(job_field "$JOB" status)" == "completed" ]] ||
	fail "private-address push target: job $JOB is $(job_field "$JOB" status), want completed"
deadline=$((SECONDS + 30))
until [[ -s "$SINK_PAYLOAD" ]]; do
	((SECONDS < deadline)) || fail "private-address push target: receiver got no data at $TARGET; see $CLI_LOG"
	sleep 0.2
done
grep -qF 'Warning: no output files returned from server' "$CLI_LOG" ||
	fail "private-address push target: the CLI offered a downloadable product for a network output; see $CLI_LOG"
outputs="$(api_get "/api/v1/jobs/$JOB" |
	python3 -c 'import json,sys; print(len(json.load(sys.stdin)["job"].get("output_files") or []))')"
[[ "$outputs" == "0" ]] || fail "private-address push target: the job holds $outputs downloadable output file(s), want 0"

# 同一请求再跑一次：每次都重新推流（缓存不参与网络输出；验收 worker 的缓存关闭，
# 这里只要求行为可重复，缓存豁免由实现侧断言覆盖）。
SIZE_BEFORE="$(stat -c %s "$SINK_PAYLOAD")"
rc=0
run_cli "$TARGET" || rc=$?
[[ $rc -eq 0 ]] || fail "private-address push target (repeat) failed (rc=$rc); see $CLI_LOG"
JOB2="$(job_id_from_log "$CLI_LOG")"
[[ "$(job_field "$JOB2" status)" == "completed" ]] ||
	fail "private-address push target (repeat): job $JOB2 is $(job_field "$JOB2" status), want completed"
deadline=$((SECONDS + 30))
until [[ "$(stat -c %s "$SINK_PAYLOAD")" -gt "$SIZE_BEFORE" ]]; do
	((SECONDS < deadline)) || fail "repeat request did not push again"
	sleep 0.2
done

# ---------------------------------------------------------------- denied literals
submit_status() {
	local output="$1"
	curl -sS -o "$TMP_ROOT/submit.json" -w '%{http_code}' \
		-H "Authorization: Bearer $AUTH_TOKEN" -H 'Content-Type: application/json' \
		-d "{\"input_files\":[\"http://example.test/in.mp4\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\"],\"output_filename\":\"$output\"}" \
		"$(acceptance_server_url)/api/v1/jobs"
}

BEFORE="$(job_count)"
# 末尾四条是 inet_aton 数值写法：ffmpeg 用 C 库解析，它们与点分写法是同一
# 地址（127.1 / 2130706433 / 0x7f000001 → 回环，0xa9fea9fe → 链路本地）。
for denied in "rtmp://127.0.0.1:1935/live/x" "udp://169.254.169.254:9/stream" \
	"srt://0.0.0.0:9000" "udp://224.0.0.1:9/stream" "rtmp://100.64.0.1:1935/live/x" \
	"udp://127.1:9/stream" "udp://2130706433:9/stream" "udp://0x7f000001:9/stream" \
	"udp://0xa9fea9fe:9/stream"; do
	code="$(submit_status "$denied")"
	[[ "$code" == "400" ]] || fail "denied literal address $denied: submit status $code, want 400"
	code_field="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("code",""))' "$TMP_ROOT/submit.json")"
	[[ "$code_field" == "invalid_request" ]] ||
		fail "denied literal address $denied: error code $code_field, want invalid_request"
done
[[ "$(job_count)" == "$BEFORE" ]] || fail "denied literal addresses: the job list grew"

# ------------------------------------------------- host name resolving to loopback
LOOP_PORTFILE="$TMP_ROOT/loop.port"
LOOP_PAYLOAD="$TMP_ROOT/loop.bin"
: >"$LOOP_PAYLOAD"
start_udp_sink "127.0.0.1" "$LOOP_PORTFILE" "$LOOP_PAYLOAD"
wait_portfile "$LOOP_PORTFILE"
LOOP_PORT="$(cat "$LOOP_PORTFILE")"

rc=0
run_cli "udp://localhost:$LOOP_PORT/stream" || rc=$?
[[ $rc -eq 1 ]] || fail "host name resolving to loopback: rc=$rc, want 1; see $CLI_LOG"
grep -qF 'Job failed: [INFRA]' "$CLI_LOG" ||
	fail "host name resolving to loopback: missing the INFRA terminal line; see $CLI_LOG"
JOB3="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB3" ]] || fail "host name resolving to loopback: no job id in the CLI log"
[[ "$(job_field "$JOB3" failure_type)" == "INFRA" ]] ||
	fail "host name resolving to loopback: failure_type $(job_field "$JOB3" failure_type), want INFRA"
[[ ! -s "$LOOP_PAYLOAD" ]] ||
	fail "host name resolving to loopback: the target received $(stat -c %s "$LOOP_PAYLOAD") byte(s), want none"
details="$(api_get "/api/v1/jobs/$JOB3" |
	python3 -c 'import json,sys; job=json.load(sys.stdin)["job"]; print((job.get("failure_details") or "") + " " + (job.get("error") or ""))')"
grep -qF 'not permitted' <<<"$details" ||
	fail "host name resolving to loopback: the failure does not come from the address policy ($details)"
if grep -qF 'Executing ffmpeg' "$WORKER_LOG" && grep -qF "udp://localhost:$LOOP_PORT" "$WORKER_LOG"; then
	fail "host name resolving to loopback: ffmpeg was started for a denied target"
fi

# 判定不出的推流目标同样不得放行（fail-closed）：主机名解析失败时作业失败
# INFRA，ffmpeg 不启动，目标端收不到任何字节。
LOOP2_PORTFILE="$TMP_ROOT/loop2.port"
LOOP2_PAYLOAD="$TMP_ROOT/loop2.bin"
: >"$LOOP2_PAYLOAD"
start_udp_sink "127.0.0.1" "$LOOP2_PORTFILE" "$LOOP2_PAYLOAD"
wait_portfile "$LOOP2_PORTFILE"
LOOP2_PORT="$(cat "$LOOP2_PORTFILE")"

rc=0
run_cli "udp://no-such-target.invalid:$LOOP2_PORT/stream" || rc=$?
[[ $rc -eq 1 ]] || fail "unresolvable push target: rc=$rc, want 1; see $CLI_LOG"
grep -qF 'Job failed: [INFRA]' "$CLI_LOG" ||
	fail "unresolvable push target: missing the INFRA terminal line; see $CLI_LOG"
JOB4="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB4" ]] || fail "unresolvable push target: no job id in the CLI log"
[[ "$(job_field "$JOB4" failure_type)" == "INFRA" ]] ||
	fail "unresolvable push target: failure_type $(job_field "$JOB4" failure_type), want INFRA"
[[ ! -s "$LOOP2_PAYLOAD" ]] ||
	fail "unresolvable push target: the target received $(stat -c %s "$LOOP2_PAYLOAD") byte(s), want none"
if grep -qF 'Executing ffmpeg' "$WORKER_LOG" && grep -qF "no-such-target.invalid:$LOOP2_PORT" "$WORKER_LOG"; then
	fail "unresolvable push target: ffmpeg was started for a target the execution side could not judge"
fi

# ------------------------------------------------------- non-address shapes
BEFORE="$(job_count)"
for bogus in "file:///etc/passwd" "file://host/tmp/x.mp4" "rtmp://" "rtmp:///only/path"; do
	code="$(submit_status "$bogus")"
	[[ "$code" == "400" ]] || fail "non-address target $bogus: submit status $code, want 400"
done
[[ "$(job_count)" == "$BEFORE" ]] || fail "non-address targets: the job list grew"

echo "PASS: a private-address protocol target is accepted and pushed (no downloadable product); denied literal addresses (including inet_aton numeric spellings) are 400 with no job row; a host name resolving into a denied class and an unresolvable target both fail INFRA before ffmpeg with the target receiving nothing"
