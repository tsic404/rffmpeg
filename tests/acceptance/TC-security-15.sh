#!/usr/bin/env bash
# TC-security-15: 判定地址即连接地址：改指向与多地址落点（AC-61）
#
# 可观察判据（docs/03-acceptance.md AC-61）:
#   - 判定后把目标主机名改指向拒绝类接收端 → 放行侧收到数据、拒绝类侧零连接，
#     作业结果与不改指向时相同（completed，CLI rc=0）。
#   - 判定得到两个放行地址、只有第二个可达 → 作业在第二个地址完成，该接收端
#     收到数据；判定集合之外的接收端零连接。
#   - 判定所得的地址全部不可达 → 作业失败 FFMPEG_ERROR，判定集合之外的接收端
#     零连接。
#
# 验收边界：「判定之后改指向」需要一个验收方可控的解析入口。本脚本在
# user+mount+network namespace 里运行：把 10.77.0.1–.3 挂到 lo，并把一份
# 临时 /etc/hosts bind mount 到系统路径，解析记录与接收端都由本用例控制，
# 不需要宿主 root，也不改动宿主 /etc/hosts。推流用 mpegts over TCP：
# TCP 建连失败是可观察的，mpegts 收到字节即证明数据真的写向了该地址。

set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"

# The isolated namespace has no external network, so build first and enter it
# only with the finished binaries. Failing here names the missing capability
# instead of letting the test run against a resolution source it cannot control.
if [[ "${RFFMPEG_TC15_NAMESPACE:-}" != "1" ]]; then
	command -v unshare >/dev/null 2>&1 ||
		{ echo "FAIL: TC-security-15 needs unshare for user+mount+network namespaces" >&2; exit 1; }
	command -v ip >/dev/null 2>&1 ||
		{ echo "FAIL: TC-security-15 needs iproute2 to assign the private receiver addresses" >&2; exit 1; }
	unshare -Urnm true 2>/dev/null ||
		{ echo "FAIL: TC-security-15 needs a permitted 'unshare -Urnm' (user, mount and network namespaces)" >&2; exit 1; }
	if [[ -n "${RFFMPEG_BIN:-}" ]]; then
		for f in rffmpeg rffmpeg-server rffmpeg-worker; do
			[[ -x "$RFFMPEG_BIN/$f" ]] || { echo "FAIL: RFFMPEG_BIN=$RFFMPEG_BIN lacks $f" >&2; exit 1; }
		done
	else
		RFFMPEG_BIN="$REPO_ROOT/bin"
		make -C "$REPO_ROOT" build >/dev/null
	fi
	exec unshare -Urnm env RFFMPEG_TC15_NAMESPACE=1 RFFMPEG_BIN="$RFFMPEG_BIN" \
		"${BASH:-bash}" "${BASH_SOURCE[0]}" "$@"
fi

ENV_DIR="$SELF_DIR"
source "$ENV_DIR/_env.sh"

build_binaries

# Private, allowed addresses on loopback of this namespace. Loopback itself is
# a denied class, so the receivers must live on RFC1918 addresses.
ip link set lo up
for host in 10.77.0.1 10.77.0.2 10.77.0.3; do
	ip addr add "$host/32" dev lo 2>/dev/null || true
done

HOSTS="$TMP_ROOT/hosts"
printf '127.0.0.1 localhost\n' >"$HOSTS"
mount --bind "$HOSTS" /etc/hosts

set_hosts() {
	printf '%s\n' "$@" >"$HOSTS"
}

# Go's resolver caches /etc/hosts for a few seconds. Wait one refresh window
# after rewriting it so the next judgement definitely reads the controlled
# entries instead of the previous file's snapshot.
settle_hosts() {
	sleep 6
}

# start_tcp_receiver <bind-ip> <port|0> <portfile> <logfile> <payload>
# Records each accepted connection (proof of a connection landing) and every
# byte received (proof of data delivery).
start_tcp_receiver() {
	local bind_ip="$1" port="$2" portfile="$3" logfile="$4" payload="$5"
	python3 - "$bind_ip" "$port" "$portfile" "$logfile" "$payload" <<'PY' &
import socket, sys

host, port, portfile, logfile, payload = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5]
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, port))
sock.listen(8)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))
with open(logfile, "a", buffering=1) as log, open(payload, "ab", buffering=0) as sink:
    while True:
        conn, peer = sock.accept()
        log.write("accept %s:%s\n" % peer)
        while True:
            try:
                data = conn.recv(65536)
            except OSError:
                break
            if not data:
                break
            sink.write(data)
        conn.close()
PY
	echo "$!" >>"$PIDFILE"
}

# start_single_tcp_receiver mirrors a native `listen=1` receiver: it accepts
# exactly one connection, closes the listening socket, and consumes that
# connection to EOF. An address-selection probe that opens and closes a
# connection first would consume the only accept and make the real push fail.
start_single_tcp_receiver() {
	local bind_ip="$1" portfile="$2" logfile="$3" payload="$4"
	python3 - "$bind_ip" "$portfile" "$logfile" "$payload" <<'PY' &
import socket, sys

host, portfile, logfile, payload = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, 0))
sock.listen(1)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))
conn, peer = sock.accept()
sock.close()
with open(logfile, "a", buffering=1) as log:
    log.write("accept %s:%s\n" % peer)
    with open(payload, "wb", buffering=0) as sink:
        while True:
            data = conn.recv(65536)
            if not data:
                break
            sink.write(data)
    log.write("done\n")
conn.close()
PY
	echo "$!" >>"$PIDFILE"
}

# start_partial_tcp_receiver accepts one connection, consumes a fixed prefix of
# the stream, then resets the socket. It proves that media already delivered to
# one judged address cannot be sent again to the next one.
start_partial_tcp_receiver() {
	local bind_ip="$1" min_bytes="$2" portfile="$3" logfile="$4"
	python3 - "$bind_ip" "$min_bytes" "$portfile" "$logfile" <<'PY' &
import socket, struct, sys

host, limit, portfile, logfile = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, 0))
sock.listen(1)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))
conn, peer = sock.accept()
total = 0
while total < limit:
    data = conn.recv(65536)
    if not data:
        break
    total += len(data)
with open(logfile, "a", buffering=1) as log:
    log.write("accept %s:%s\n" % peer)
    log.write("bytes %d\n" % total)
conn.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
conn.close()
sock.close()
PY
	echo "$!" >>"$PIDFILE"
}

# start_udp_counter records every datagram sent to an address outside the
# judged landing set, so a connectionless protocol still has a zero-connection
# observation.
start_udp_counter() {
	local bind_ip="$1" port="$2" logfile="$3" countfile="$4"
	python3 - "$bind_ip" "$port" "$logfile" "$countfile" <<'PY' &
import socket, sys

host, port, logfile, countfile = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, port))
count = 0
with open(logfile, "a", buffering=1) as log:
    while True:
        _, peer = sock.recvfrom(65535)
        count += 1
        with open(countfile, "w") as f:
            f.write(str(count))
        log.write("datagram %s:%s\n" % peer)
PY
	echo "$!" >>"$PIDFILE"
}

wait_portfile() {
	local portfile="$1" deadline=$((SECONDS + 10))
	until [[ -s "$portfile" ]]; do
		((SECONDS < deadline)) || fail "TCP receiver did not start: $portfile"
		sleep 0.1
	done
}

payload_size() {
	stat -c %s "$1" 2>/dev/null || echo 0
}

accept_count() {
	grep -c '^accept ' "$1" 2>/dev/null || true
}

wait_payload_growth() {
	local payload="$1" before="$2" label="$3" forbidden="${4:-}"
	local deadline=$((SECONDS + 30))
	until [[ "$(payload_size "$payload")" -gt "$before" ]]; do
		if [[ -n "$forbidden" && "$(payload_size "$forbidden")" -gt 0 ]]; then
			fail "$label: data landed on a receiver outside the judged landing address"
		fi
		((SECONDS < deadline)) || fail "$label: receiver got no data"
		sleep 0.2
	done
}

# run_cli submits one push job and leaves its stderr in $CLI_LOG. Extra
# arguments are placed before the output target.
run_cli() {
	local target="$1" format="$2"
	shift 2
	CLI_LOG="$TMP_ROOT/cli-$(printf '%s' "$target" | sha256sum | cut -c1-8).log"
	set +e
	"$RFFMPEG_BIN/rffmpeg" \
		-i "$MEDIA" -c:v libx264 -preset ultrafast -t 1 -f "$format" "$@" "$target" 2>"$CLI_LOG"
	local rc=$?
	set -e
	return $rc
}

expect_completed() {
	local label="$1" rc="$2"
	[[ $rc -eq 0 ]] || fail "$label: rc=$rc, want 0; see $CLI_LOG"
	JOB="$(job_id_from_log "$CLI_LOG")"
	[[ -n "$JOB" ]] || fail "$label: no job id in the CLI log"
	[[ "$(job_field "$JOB" status)" == "completed" ]] ||
		fail "$label: job $JOB is $(job_field "$JOB" status), want completed; see $CLI_LOG"
}

# ffmpeg wrapper: a flag file marks the rebinding window. While it exists every
# ffmpeg invocation sees the denied /etc/hosts entry, then the original entry is
# restored. A worker that hands the host name to ffmpeg connects to the denied
# receiver; a worker that hands over the judged address cannot.
FFMPEG_WRAPPER="$TMP_ROOT/ffmpeg-tc15"
REAL_FFMPEG="$(command -v ffmpeg)"
REBIND_HOSTS="$TMP_ROOT/hosts-rebound"
REBIND_FLAG="$TMP_ROOT/rebind.flag"
cat >"$FFMPEG_WRAPPER" <<'WRAPPER'
#!/usr/bin/env bash
set -uo pipefail
hosts="${TC15_HOSTS:?}"
flag="${TC15_REBIND_FLAG:?}"
rebind="${TC15_REBIND_HOSTS:?}"
real="${TC15_REAL_FFMPEG:?}"
backup="$hosts.tc15-backup"
if [[ -f "$flag" ]]; then
	cp -- "$hosts" "$backup"
	cat -- "$rebind" >"$hosts"
fi
"$real" "$@"
rc=$?
if [[ -f "$backup" ]]; then
	cat -- "$backup" >"$hosts"
	rm -f -- "$backup"
fi
if [[ -f "${TC15_EXIT1_FLAG:-}" && $rc -ne 0 ]]; then
	rc=1
fi
exit "$rc"
WRAPPER
chmod +x "$FFMPEG_WRAPPER"
printf '127.0.0.1 localhost\n127.0.0.1 tc15-push.test\n' >"$REBIND_HOSTS"

start_server
TC15_TEMP_BASE="$TMP_ROOT/worker-tc15%dir:\"quote\"\\slash"
start_worker tc15-worker \
	RFFMPEG_TEMP_DIR="$TC15_TEMP_BASE" \
	RFFMPEG_FFMPEG_PATH="$FFMPEG_WRAPPER" \
	TC15_HOSTS="$HOSTS" \
	TC15_REBIND_HOSTS="$REBIND_HOSTS" \
	TC15_REBIND_FLAG="$REBIND_FLAG" \
	TC15_REAL_FFMPEG="$REAL_FFMPEG" \
	TC15_EXIT1_FLAG="$TMP_ROOT/exit1.flag" \
	GODEBUG=netdns=go
wait_ready

MEDIA="$(make_media net-output.mp4 1)"

# ---------------------------------------------------------------- 改指向
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-push.test'
settle_hosts

PUSH_PORTFILE="$TMP_ROOT/push.port"
PUSH_LOG="$TMP_ROOT/push.accepts"
PUSH_PAYLOAD="$TMP_ROOT/push.bin"
: >"$PUSH_LOG"
: >"$PUSH_PAYLOAD"
start_tcp_receiver 10.77.0.2 0 "$PUSH_PORTFILE" "$PUSH_LOG" "$PUSH_PAYLOAD"
wait_portfile "$PUSH_PORTFILE"
PUSH_PORT="$(cat "$PUSH_PORTFILE")"

DENIED_PORTFILE="$TMP_ROOT/denied.port"
DENIED_LOG="$TMP_ROOT/denied.accepts"
DENIED_PAYLOAD="$TMP_ROOT/denied.bin"
: >"$DENIED_LOG"
: >"$DENIED_PAYLOAD"
start_tcp_receiver 127.0.0.1 "$PUSH_PORT" "$DENIED_PORTFILE" "$DENIED_LOG" "$DENIED_PAYLOAD"
wait_portfile "$DENIED_PORTFILE"

PUSH_TARGET="tcp://tc15-push.test:$PUSH_PORT/stream"

# Control run: the host name resolves to the allowed receiver for both the
# judgement and ffmpeg.
rc=0
run_cli "$PUSH_TARGET" mpegts || rc=$?
expect_completed "control push" "$rc"
wait_payload_growth "$PUSH_PAYLOAD" 0 "control push" "$DENIED_PAYLOAD"
[[ "$(accept_count "$DENIED_LOG")" == "0" ]] ||
	fail "control push: the denied receiver was contacted"

# Rebind run: judgement still sees 10.77.0.2; before ffmpeg connects, the same
# host name is rewritten to the denied loopback receiver.
SIZE_BEFORE="$(payload_size "$PUSH_PAYLOAD")"
: >"$REBIND_FLAG"
rc=0
run_cli "$PUSH_TARGET" mpegts || rc=$?
rm -f "$REBIND_FLAG"
expect_completed "rebound push" "$rc"
wait_payload_growth "$PUSH_PAYLOAD" "$SIZE_BEFORE" "rebound push" "$DENIED_PAYLOAD"
[[ "$(accept_count "$DENIED_LOG")" == "0" ]] ||
	fail "rebound push: ffmpeg connected to the post-judgement denied address"
grep -qF '10.77.0.2 tc15-push.test' "$HOSTS" ||
	fail "rebound push: the wrapper did not restore the controlled hosts file"
echo "PASS: [rebind] payload_bytes=$(payload_size "$PUSH_PAYLOAD") denied_accepts=$(accept_count "$DENIED_LOG")"

# ------------------------------------------------------------ 多地址落点
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-multi.test' '10.77.0.2 tc15-multi.test'
settle_hosts

MULTI_PORTFILE="$TMP_ROOT/multi.port"
MULTI_LOG="$TMP_ROOT/multi.accepts"
MULTI_PAYLOAD="$TMP_ROOT/multi.bin"
: >"$MULTI_LOG"
: >"$MULTI_PAYLOAD"
start_tcp_receiver 10.77.0.2 0 "$MULTI_PORTFILE" "$MULTI_LOG" "$MULTI_PAYLOAD"
wait_portfile "$MULTI_PORTFILE"
MULTI_PORT="$(cat "$MULTI_PORTFILE")"

TRAP_PORTFILE="$TMP_ROOT/trap.port"
TRAP_LOG="$TMP_ROOT/trap.accepts"
TRAP_PAYLOAD="$TMP_ROOT/trap.bin"
: >"$TRAP_LOG"
: >"$TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$MULTI_PORT" "$TRAP_PORTFILE" "$TRAP_LOG" "$TRAP_PAYLOAD"
wait_portfile "$TRAP_PORTFILE"

rc=0
run_cli "tcp://tc15-multi.test:$MULTI_PORT/stream" mpegts || rc=$?
expect_completed "multi-address push" "$rc"
wait_payload_growth "$MULTI_PAYLOAD" 0 "multi-address push" "$TRAP_PAYLOAD"
[[ "$(accept_count "$TRAP_LOG")" == "0" ]] ||
	fail "multi-address push: a receiver outside the judged set was contacted"
echo "PASS: [multi-address] payload_bytes=$(payload_size "$MULTI_PAYLOAD") outside_accepts=$(accept_count "$TRAP_LOG")"

# ffmpeg 5.1 reports a connect failure as exit code 1, not 256-errno. The
# fallback must come from the relay's dial observation, so the generic exit
# code still reaches the second judged address.
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-exit1.test' '10.77.0.2 tc15-exit1.test'
settle_hosts

EXIT1_PORTFILE="$TMP_ROOT/exit1.port"
EXIT1_LOG="$TMP_ROOT/exit1.accepts"
EXIT1_PAYLOAD="$TMP_ROOT/exit1.bin"
: >"$EXIT1_LOG"
: >"$EXIT1_PAYLOAD"
start_tcp_receiver 10.77.0.2 0 "$EXIT1_PORTFILE" "$EXIT1_LOG" "$EXIT1_PAYLOAD"
wait_portfile "$EXIT1_PORTFILE"
EXIT1_PORT="$(cat "$EXIT1_PORTFILE")"

EXIT1_TRAP_LOG="$TMP_ROOT/exit1-trap.accepts"
EXIT1_TRAP_PAYLOAD="$TMP_ROOT/exit1-trap.bin"
: >"$EXIT1_TRAP_LOG"
: >"$EXIT1_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$EXIT1_PORT" "$TMP_ROOT/exit1-trap.port" "$EXIT1_TRAP_LOG" "$EXIT1_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/exit1-trap.port"

: >"$TMP_ROOT/exit1.flag"
rc=0
run_cli "tcp://tc15-exit1.test:$EXIT1_PORT/stream" mpegts || rc=$?
rm -f "$TMP_ROOT/exit1.flag"
expect_completed "exit-1 multi-address push" "$rc"
wait_payload_growth "$EXIT1_PAYLOAD" 0 "exit-1 multi-address push" "$EXIT1_TRAP_PAYLOAD"
[[ "$(accept_count "$EXIT1_TRAP_LOG")" == "0" ]] ||
	fail "exit-1 multi-address push: a receiver outside the judged set was contacted"
echo "PASS: [exit1-multi-address] payload_bytes=$(payload_size "$EXIT1_PAYLOAD") outside_accepts=$(accept_count "$EXIT1_TRAP_LOG")"

# ------------------------------------------- 多地址落点（quiet 日志）
# The chosen address must not depend on ffmpeg's stderr: with -loglevel quiet
# the first address still refuses and the second still has to receive the job.
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-quiet.test' '10.77.0.2 tc15-quiet.test'
settle_hosts

QUIET_PORTFILE="$TMP_ROOT/quiet.port"
QUIET_LOG="$TMP_ROOT/quiet.accepts"
QUIET_PAYLOAD="$TMP_ROOT/quiet.bin"
: >"$QUIET_LOG"
: >"$QUIET_PAYLOAD"
start_tcp_receiver 10.77.0.2 0 "$QUIET_PORTFILE" "$QUIET_LOG" "$QUIET_PAYLOAD"
wait_portfile "$QUIET_PORTFILE"
QUIET_PORT="$(cat "$QUIET_PORTFILE")"

QUIET_TRAP_LOG="$TMP_ROOT/quiet-trap.accepts"
QUIET_TRAP_PAYLOAD="$TMP_ROOT/quiet-trap.bin"
: >"$QUIET_TRAP_LOG"
: >"$QUIET_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$QUIET_PORT" "$TMP_ROOT/quiet-trap.port" "$QUIET_TRAP_LOG" "$QUIET_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/quiet-trap.port"

rc=0
run_cli "tcp://tc15-quiet.test:$QUIET_PORT/stream" mpegts -loglevel quiet || rc=$?
expect_completed "quiet multi-address push" "$rc"
wait_payload_growth "$QUIET_PAYLOAD" 0 "quiet multi-address push" "$QUIET_TRAP_PAYLOAD"
[[ "$(accept_count "$QUIET_TRAP_LOG")" == "0" ]] ||
	fail "quiet multi-address push: a receiver outside the judged set was contacted"
echo "PASS: [quiet-multi-address] payload_bytes=$(payload_size "$QUIET_PAYLOAD") outside_accepts=$(accept_count "$QUIET_TRAP_LOG")"

# The worker-owned progress name is relative to the job workspace. A legal
# temporary directory containing `%`, `:`, quotes and a backslash must not move
# it, and the quiet fallback above ran from exactly this directory.
[[ "$TC15_TEMP_BASE" == *%* && "$TC15_TEMP_BASE" == *:* ]] ||
	fail "progress-path escaping: the worker temp dir does not contain the intended metacharacters: $TC15_TEMP_BASE"
[[ "$TC15_TEMP_BASE" == *'"'* && "$TC15_TEMP_BASE" == *'\'* ]] ||
	fail "progress-path escaping: the worker temp dir lacks quote/backslash coverage: $TC15_TEMP_BASE"
PROGRESS_LEFTOVERS="$(find "$TC15_TEMP_BASE" -type f -name '.push-progress-*' -print -quit)"
[[ -z "$PROGRESS_LEFTOVERS" ]] ||
	fail "progress-path escaping: worker-private progress files survived the job: $PROGRESS_LEFTOVERS"
echo "PASS: [progress-path] temp_dir=$TC15_TEMP_BASE leftover_progress=0 quiet_fallback_bytes=$(payload_size "$QUIET_PAYLOAD")"

# ------------------------------------------------ 单连接接收端（listen=1）
# A native single-connection receiver accepts once and closes its listener.
# Address selection must not consume that accept with an empty probe: the real
# connection has to be the one that carries the stream.
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-single.test'
settle_hosts

SINGLE_PORTFILE="$TMP_ROOT/single.port"
SINGLE_LOG="$TMP_ROOT/single.accepts"
SINGLE_PAYLOAD="$TMP_ROOT/single.bin"
: >"$SINGLE_LOG"
: >"$SINGLE_PAYLOAD"
start_single_tcp_receiver 10.77.0.2 "$SINGLE_PORTFILE" "$SINGLE_LOG" "$SINGLE_PAYLOAD"
wait_portfile "$SINGLE_PORTFILE"
SINGLE_PORT="$(cat "$SINGLE_PORTFILE")"

SINGLE_TRAP_LOG="$TMP_ROOT/single-trap.accepts"
SINGLE_TRAP_PAYLOAD="$TMP_ROOT/single-trap.bin"
: >"$SINGLE_TRAP_LOG"
: >"$SINGLE_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$SINGLE_PORT" "$TMP_ROOT/single-trap.port" "$SINGLE_TRAP_LOG" "$SINGLE_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/single-trap.port"

rc=0
run_cli "tcp://tc15-single.test:$SINGLE_PORT/stream" mpegts || rc=$?
expect_completed "single-connection push" "$rc"
wait_payload_growth "$SINGLE_PAYLOAD" 0 "single-connection push" "$SINGLE_TRAP_PAYLOAD"
[[ "$(accept_count "$SINGLE_LOG")" == "1" ]] ||
	fail "single-connection push: receiver accepted $(accept_count "$SINGLE_LOG") connections, want exactly 1"
[[ "$(accept_count "$SINGLE_TRAP_LOG")" == "0" ]] ||
	fail "single-connection push: a receiver outside the judged set was contacted"
echo "PASS: [single-connection] payload_bytes=$(payload_size "$SINGLE_PAYLOAD") accepts=$(accept_count "$SINGLE_LOG") outside_accepts=$(accept_count "$SINGLE_TRAP_LOG")"

# ------------------------------------------------------------ 全部不可达
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-dead.test' '10.77.0.3 tc15-dead.test'
settle_hosts

DEAD_PORTFILE="$TMP_ROOT/dead.port"
DEAD_LOG="$TMP_ROOT/dead.accepts"
DEAD_PAYLOAD="$TMP_ROOT/dead.bin"
: >"$DEAD_LOG"
: >"$DEAD_PAYLOAD"
# 10.77.0.2 is outside the judged set here: it must stay untouched.
start_tcp_receiver 10.77.0.2 0 "$DEAD_PORTFILE" "$DEAD_LOG" "$DEAD_PAYLOAD"
wait_portfile "$DEAD_PORTFILE"
DEAD_PORT="$(cat "$DEAD_PORTFILE")"

rc=0
run_cli "tcp://tc15-dead.test:$DEAD_PORT/stream" mpegts || rc=$?
[[ $rc -eq 1 ]] || fail "all-unreachable push: rc=$rc, want 1; see $CLI_LOG"
grep -qF 'Job failed: [FFMPEG_ERROR]' "$CLI_LOG" ||
	fail "all-unreachable push: missing the FFMPEG_ERROR terminal line; see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "all-unreachable push: no job id in the CLI log"
[[ "$(job_field "$JOB" failure_type)" == "FFMPEG_ERROR" ]] ||
	fail "all-unreachable push: failure_type $(job_field "$JOB" failure_type), want FFMPEG_ERROR"
[[ "$(accept_count "$DEAD_LOG")" == "0" ]] ||
	fail "all-unreachable push: a receiver outside the judged set was contacted"
echo "PASS: [all-unreachable] failure_type=$(job_field "$JOB" failure_type) outside_accepts=$(accept_count "$DEAD_LOG")"

# ------------------------------------- 媒体投递后的报告文本不得触发重推
# The first judged receiver consumes real media and then resets the socket.
# The request's own metadata says "Connection refused"; that text appears in
# ffmpeg's report command line and output metadata. The second judged address
# must receive nothing: delivered media is the authoritative stop signal.
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-dup.test' '10.77.0.2 tc15-dup.test'
settle_hosts

DUP_PORTFILE="$TMP_ROOT/dup.port"
DUP_LOG="$TMP_ROOT/dup.accepts"
: >"$DUP_LOG"
start_partial_tcp_receiver 10.77.0.1 1024 "$DUP_PORTFILE" "$DUP_LOG"
wait_portfile "$DUP_PORTFILE"
DUP_PORT="$(cat "$DUP_PORTFILE")"

DUP_SECOND_LOG="$TMP_ROOT/dup-second.accepts"
DUP_SECOND_PAYLOAD="$TMP_ROOT/dup-second.bin"
: >"$DUP_SECOND_LOG"
: >"$DUP_SECOND_PAYLOAD"
start_tcp_receiver 10.77.0.2 "$DUP_PORT" "$TMP_ROOT/dup-second.port" "$DUP_SECOND_LOG" "$DUP_SECOND_PAYLOAD"
wait_portfile "$TMP_ROOT/dup-second.port"

DUP_TRAP_LOG="$TMP_ROOT/dup-trap.accepts"
DUP_TRAP_PAYLOAD="$TMP_ROOT/dup-trap.bin"
: >"$DUP_TRAP_LOG"
: >"$DUP_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$DUP_PORT" "$TMP_ROOT/dup-trap.port" "$DUP_TRAP_LOG" "$DUP_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/dup-trap.port"

rc=0
run_cli "tcp://tc15-dup.test:$DUP_PORT/stream" mpegts -metadata 'comment=Connection refused' || rc=$?
[[ $rc -eq 1 ]] || fail "delivered-then-reset push: rc=$rc, want 1; see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "delivered-then-reset push: no job id in the CLI log"
[[ "$(job_field "$JOB" status)" == "failed" ]] ||
	fail "delivered-then-reset push: job status $(job_field "$JOB" status), want failed"
DUP_BYTES="$(awk '$1 == "bytes" { print $2 }' "$DUP_LOG" | tail -1)"
[[ "${DUP_BYTES:-0}" -gt 0 ]] ||
	fail "delivered-then-reset push: the first judged address received no media"
[[ "$(accept_count "$DUP_SECOND_LOG")" == "0" ]] ||
	fail "delivered-then-reset push: the second judged address received a duplicate connection"
[[ "$(payload_size "$DUP_SECOND_PAYLOAD")" == "0" ]] ||
	fail "delivered-then-reset push: the second judged address received duplicate media"
[[ "$(accept_count "$DUP_TRAP_LOG")" == "0" ]] ||
	fail "delivered-then-reset push: a receiver outside the judged set was contacted"
echo "PASS: [delivered-then-reset] first_bytes=$DUP_BYTES second_accepts=$(accept_count "$DUP_SECOND_LOG") second_bytes=$(payload_size "$DUP_SECOND_PAYLOAD") outside_accepts=$(accept_count "$DUP_TRAP_LOG")"

# ------------------------------------------------ HTTP 重定向不得越出集合
# A 307 from the judged receiver is a second connection chosen by ffmpeg. The
# push must refuse it instead of following it to the loopback trap.
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-http.test'
settle_hosts

HTTP_TRAP_PORTFILE="$TMP_ROOT/http-trap.port"
HTTP_TRAP_LOG="$TMP_ROOT/http-trap.accepts"
HTTP_TRAP_PAYLOAD="$TMP_ROOT/http-trap.bin"
: >"$HTTP_TRAP_LOG"
: >"$HTTP_TRAP_PAYLOAD"
start_tcp_receiver 127.0.0.1 0 "$HTTP_TRAP_PORTFILE" "$HTTP_TRAP_LOG" "$HTTP_TRAP_PAYLOAD"
wait_portfile "$HTTP_TRAP_PORTFILE"
HTTP_TRAP_PORT="$(cat "$HTTP_TRAP_PORTFILE")"

HTTP_PORTFILE="$TMP_ROOT/http.port"
HTTP_LOG="$TMP_ROOT/http.requests"
: >"$HTTP_LOG"
cat >"$TMP_ROOT/http-redirect.py" <<'PY'
import socket, sys

host, location, portfile, logfile = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, 0))
sock.listen(8)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))
with open(logfile, "a", buffering=1) as log:
    while True:
        conn, peer = sock.accept()
        conn.settimeout(2)
        request = b""
        try:
            while b"\r\n\r\n" not in request:
                chunk = conn.recv(4096)
                if not chunk:
                    break
                request += chunk
        except OSError:
            pass
        if b"\r\n\r\n" not in request:
            conn.close()
            continue
        log.write("POST /stream\n")
        conn.sendall((
            "HTTP/1.1 307 Temporary Redirect\r\n"
            "Location: %s\r\n"
            "Content-Length: 0\r\n"
            "Connection: close\r\n\r\n" % location
        ).encode())
        conn.close()
PY
python3 "$TMP_ROOT/http-redirect.py" 10.77.0.2 \
	"http://127.0.0.1:$HTTP_TRAP_PORT/stream" "$HTTP_PORTFILE" "$HTTP_LOG" \
	>>"$TMP_ROOT/http-redirect.log" 2>&1 &
echo "$!" >>"$PIDFILE"
wait_portfile "$HTTP_PORTFILE"
HTTP_PORT="$(cat "$HTTP_PORTFILE")"

rc=0
run_cli "http://tc15-http.test:$HTTP_PORT/stream" mpegts -send_expect_100 1 || rc=$?
[[ $rc -eq 1 ]] || fail "HTTP redirect push: rc=$rc, want 1; see $CLI_LOG"
grep -qF 'Job failed: [FFMPEG_ERROR]' "$CLI_LOG" ||
	fail "HTTP redirect push: missing the FFMPEG_ERROR terminal line; see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "HTTP redirect push: no job id in the CLI log"
[[ "$(job_field "$JOB" failure_type)" == "FFMPEG_ERROR" ]] ||
	fail "HTTP redirect push: failure_type $(job_field "$JOB" failure_type), want FFMPEG_ERROR"
grep -qF 'POST /stream' "$HTTP_LOG" ||
	fail "HTTP redirect push: the judged receiver never saw the push request"
[[ "$(accept_count "$HTTP_TRAP_LOG")" == "0" ]] ||
	fail "HTTP redirect push: ffmpeg followed the redirect to a receiver outside the judged set"
echo "PASS: [http-307] judged_requests=$(grep -cF 'POST /stream' "$HTTP_LOG") failure_type=$(job_field "$JOB" failure_type) outside_accepts=$(accept_count "$HTTP_TRAP_LOG")"

# ------------------------------------------ RTMPT 隧道内重定向不得越出集合
# FFmpeg's RTMPT client allocates its own HTTP context for POST /open/1 and
# follows a redirect there. The judged receiver answers 307 with a loopback
# Location; the worker must keep that follow-up connection inside the judged
# set, so the trap still accepts nothing.
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-rtmpt.test'
settle_hosts

RTMPT_TRAP_PORTFILE="$TMP_ROOT/rtmpt-trap.port"
RTMPT_TRAP_LOG="$TMP_ROOT/rtmpt-trap.accepts"
RTMPT_TRAP_PAYLOAD="$TMP_ROOT/rtmpt-trap.bin"
: >"$RTMPT_TRAP_LOG"
: >"$RTMPT_TRAP_PAYLOAD"
start_tcp_receiver 127.0.0.1 0 "$RTMPT_TRAP_PORTFILE" "$RTMPT_TRAP_LOG" "$RTMPT_TRAP_PAYLOAD"
wait_portfile "$RTMPT_TRAP_PORTFILE"
RTMPT_TRAP_PORT="$(cat "$RTMPT_TRAP_PORTFILE")"

RTMPT_PORTFILE="$TMP_ROOT/rtmpt.port"
RTMPT_LOG="$TMP_ROOT/rtmpt.requests"
: >"$RTMPT_LOG"
cat >"$TMP_ROOT/rtmpt-redirect.py" <<'PY'
import socket, sys

host, location, portfile, logfile = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, 0))
sock.listen(8)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))
with open(logfile, "a", buffering=1) as log:
    while True:
        conn, _ = sock.accept()
        conn.settimeout(2)
        request = b""
        try:
            while b"\r\n\r\n" not in request:
                chunk = conn.recv(4096)
                if not chunk:
                    break
                request += chunk
        except OSError:
            pass
        if b"\r\n\r\n" not in request:
            conn.close()
            continue
        log.write(request.split(b"\r\n", 1)[0].decode("utf-8", "replace") + "\n")
        conn.sendall((
            "HTTP/1.1 307 Temporary Redirect\r\n"
            "Location: %s\r\n"
            "Content-Length: 0\r\n"
            "Connection: close\r\n\r\n" % location
        ).encode())
        conn.close()
PY
python3 "$TMP_ROOT/rtmpt-redirect.py" 10.77.0.2 \
	"http://127.0.0.1:$RTMPT_TRAP_PORT/open/1" "$RTMPT_PORTFILE" "$RTMPT_LOG" \
	>>"$TMP_ROOT/rtmpt-redirect.log" 2>&1 &
echo "$!" >>"$PIDFILE"
wait_portfile "$RTMPT_PORTFILE"
RTMPT_PORT="$(cat "$RTMPT_PORTFILE")"

rc=0
run_cli "rtmpt://tc15-rtmpt.test:$RTMPT_PORT/live/key" flv || rc=$?
[[ $rc -eq 1 ]] || fail "RTMPT redirect push: rc=$rc, want 1; see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "RTMPT redirect push: no job id in the CLI log"
[[ "$(job_field "$JOB" status)" == "failed" ]] ||
	fail "RTMPT redirect push: job status $(job_field "$JOB" status), want failed"
grep -qF '/open/1' "$RTMPT_LOG" ||
	fail "RTMPT redirect push: the judged receiver never saw the tunnel's /open/1 request"
[[ "$(accept_count "$RTMPT_TRAP_LOG")" == "0" ]] ||
	fail "RTMPT redirect push: the tunnel followed the redirect to a receiver outside the judged set"
echo "PASS: [rtmpt-307] judged_requests=$(grep -cF '/open/1' "$RTMPT_LOG") outside_accepts=$(accept_count "$RTMPT_TRAP_LOG")"

# ------------------------------- RTMPT 的 HTTP Host 保留请求域名（按域名分流）
# The receiver only serves the request hostname the user asked for. The tunnel
# still dials the judged address; the HTTP Host and /open/1 path arrive
# unchanged. A redirect to the loopback trap keeps the zero-outside assertion.
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-rtmpt-host.test'
settle_hosts

RTMPT_HOST_TRAP_PORTFILE="$TMP_ROOT/rtmpt-host-trap.port"
RTMPT_HOST_TRAP_LOG="$TMP_ROOT/rtmpt-host-trap.accepts"
RTMPT_HOST_TRAP_PAYLOAD="$TMP_ROOT/rtmpt-host-trap.bin"
: >"$RTMPT_HOST_TRAP_LOG"
: >"$RTMPT_HOST_TRAP_PAYLOAD"
start_tcp_receiver 127.0.0.1 0 "$RTMPT_HOST_TRAP_PORTFILE" "$RTMPT_HOST_TRAP_LOG" "$RTMPT_HOST_TRAP_PAYLOAD"
wait_portfile "$RTMPT_HOST_TRAP_PORTFILE"
RTMPT_HOST_TRAP_PORT="$(cat "$RTMPT_HOST_TRAP_PORTFILE")"

RTMPT_HOST_PORTFILE="$TMP_ROOT/rtmpt-host.port"
RTMPT_HOST_LOG="$TMP_ROOT/rtmpt-host.requests"
: >"$RTMPT_HOST_LOG"
cat >"$TMP_ROOT/rtmpt-host.py" <<'PY'
import socket, sys

host, expected_host, location, portfile, logfile = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, 0))
sock.listen(8)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))
expected_host = expected_host.replace("%PORT%", str(sock.getsockname()[1]))
with open(logfile, "a", buffering=1) as log:
    while True:
        conn, _ = sock.accept()
        conn.settimeout(2)
        request = b""
        try:
            while b"\r\n\r\n" not in request:
                chunk = conn.recv(4096)
                if not chunk:
                    break
                request += chunk
        except OSError:
            pass
        if b"\r\n\r\n" not in request:
            conn.close()
            continue
        lines = request.split(b"\r\n")
        request_line = lines[0].decode("utf-8", "replace")
        headers = {}
        for line in lines[1:]:
            if b":" not in line:
                continue
            key, value = line.split(b":", 1)
            headers[key.strip().lower()] = value.strip().decode("utf-8", "replace")
        received_host = headers.get(b"host", "")
        log.write("request=%s host=%s\n" % (request_line, received_host))
        if request_line.startswith("POST /open/1 ") and received_host == expected_host:
            conn.sendall((
                "HTTP/1.1 307 Temporary Redirect\r\n"
                "Location: %s\r\n"
                "Content-Length: 0\r\n"
                "Connection: close\r\n\r\n" % location
            ).encode())
        else:
            conn.sendall(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        conn.close()
PY
python3 "$TMP_ROOT/rtmpt-host.py" 10.77.0.2 \
	"tc15-rtmpt-host.test:%PORT%" \
	"http://127.0.0.1:$RTMPT_HOST_TRAP_PORT/open/1" "$RTMPT_HOST_PORTFILE" "$RTMPT_HOST_LOG" \
	>>"$TMP_ROOT/rtmpt-host.log" 2>&1 &
echo "$!" >>"$PIDFILE"
wait_portfile "$RTMPT_HOST_PORTFILE"
RTMPT_HOST_PORT="$(cat "$RTMPT_HOST_PORTFILE")"

rc=0
run_cli "rtmpt://tc15-rtmpt-host.test:$RTMPT_HOST_PORT/live/key" flv || rc=$?
[[ $rc -eq 1 ]] || fail "RTMPT host routing: rc=$rc, want 1; see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "RTMPT host routing: no job id in the CLI log"
[[ "$(job_field "$JOB" status)" == "failed" ]] ||
	fail "RTMPT host routing: job status $(job_field "$JOB" status), want failed"
grep -qF "POST /open/1 HTTP/1.1" "$RTMPT_HOST_LOG" ||
	fail "RTMPT host routing: the judged receiver never saw POST /open/1"
grep -qF "host=tc15-rtmpt-host.test:$RTMPT_HOST_PORT" "$RTMPT_HOST_LOG" ||
	fail "RTMPT host routing: the judged receiver saw the wrong HTTP Host; log: $(cat "$RTMPT_HOST_LOG")"
[[ "$(accept_count "$RTMPT_HOST_TRAP_LOG")" == "0" ]] ||
	fail "RTMPT host routing: the tunnel followed the redirect to a receiver outside the judged set"
echo "PASS: [rtmpt-host] judged_requests=$(grep -cF 'POST /open/1' "$RTMPT_HOST_LOG") outside_accepts=$(accept_count "$RTMPT_HOST_TRAP_LOG")"

# ------------------------------------------------------ SRT 第二地址回退
# SRT is UDP-carried but has a caller handshake. A handshake failure on the
# first judged address must move to the next judged address; the second address
# runs a real FFmpeg SRT listener and must receive media.
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-srt.test' '10.77.0.2 tc15-srt.test'
settle_hosts

pick_udp_port() {
	python3 - "$1" <<'PY'
import socket, sys

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.bind((sys.argv[1], 0))
print(sock.getsockname()[1])
sock.close()
PY
}

SRT_PORT="$(pick_udp_port 10.77.0.2)"
SRT_LISTENER_LOG="$TMP_ROOT/srt-listener.log"
SRT_RECEIVED="$TMP_ROOT/srt-received.ts"
: >"$SRT_RECEIVED"
"$REAL_FFMPEG" -hide_banner -loglevel warning -nostdin -y \
	-f mpegts -i "srt://10.77.0.2:$SRT_PORT?mode=listener&messageapi=1&transtype=live&latency=500" \
	-c copy -flush_packets 1 -f mpegts "$SRT_RECEIVED" >>"$SRT_LISTENER_LOG" 2>&1 &
SRT_LISTENER_PID=$!
echo "$SRT_LISTENER_PID" >>"$PIDFILE"
sleep 1

SRT_TRAP_LOG="$TMP_ROOT/srt-trap.datagrams"
SRT_TRAP_COUNT="$TMP_ROOT/srt-trap.count"
: >"$SRT_TRAP_LOG"
echo 0 >"$SRT_TRAP_COUNT"
start_udp_counter 10.77.0.3 "$SRT_PORT" "$SRT_TRAP_LOG" "$SRT_TRAP_COUNT"
sleep 0.3

rc=0
run_cli "srt://tc15-srt.test:$SRT_PORT?mode=caller&connect_timeout=1000&messageapi=1&transtype=live&latency=500" mpegts || rc=$?
expect_completed "SRT second-address push" "$rc"
deadline=$((SECONDS + 30))
until [[ "$(payload_size "$SRT_RECEIVED")" -gt 0 ]]; do
	((SECONDS < deadline)) || fail "SRT second-address push: the second judged address received no media"
	sleep 0.2
done
# The listener reports the sender's close as an I/O error after the media has
# been written; wait for it to finish so the byte count is not cut short.
for _ in $(seq 1 50); do
	kill -0 "$SRT_LISTENER_PID" 2>/dev/null || break
	sleep 0.2
done
SRT_BYTES="$(payload_size "$SRT_RECEIVED")"
kill "$SRT_LISTENER_PID" 2>/dev/null || true
wait "$SRT_LISTENER_PID" 2>/dev/null || true
[[ "$(cat "$SRT_TRAP_COUNT")" == "0" ]] ||
	fail "SRT second-address push: a receiver outside the judged set received datagrams"
echo "PASS: [srt-second-address] received_bytes=$SRT_BYTES outside_datagrams=$(cat "$SRT_TRAP_COUNT")"

# The same protocol with every judged address unreachable must fail as
# FFMPEG_ERROR and still leave the outside receiver untouched.
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-srt-dead.test' '10.77.0.2 tc15-srt-dead.test'
settle_hosts
echo 0 >"$SRT_TRAP_COUNT"

rc=0
run_cli "srt://tc15-srt-dead.test:$SRT_PORT?mode=caller&connect_timeout=500&messageapi=1&transtype=live&latency=500" mpegts || rc=$?
[[ $rc -eq 1 ]] || fail "SRT all-unreachable push: rc=$rc, want 1; see $CLI_LOG"
grep -qF 'Job failed: [FFMPEG_ERROR]' "$CLI_LOG" ||
	fail "SRT all-unreachable push: missing the FFMPEG_ERROR terminal line; see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "SRT all-unreachable push: no job id in the CLI log"
[[ "$(job_field "$JOB" failure_type)" == "FFMPEG_ERROR" ]] ||
	fail "SRT all-unreachable push: failure_type $(job_field "$JOB" failure_type), want FFMPEG_ERROR"
[[ "$(cat "$SRT_TRAP_COUNT")" == "0" ]] ||
	fail "SRT all-unreachable push: a receiver outside the judged set received datagrams"
echo "PASS: [srt-all-unreachable] failure_type=$(job_field "$JOB" failure_type) outside_datagrams=$(cat "$SRT_TRAP_COUNT")"

# ------------------------------------------------------- RTSP 数据通道落点
# SETUP can point the RTP/RTCP data channel at a different address than the
# control connection, including loopback. The push forces TCP interleaved, so
# ffmpeg either carries media on the judged control connection or rejects a
# reply that negotiates another lower transport before opening any socket.
RTSP_MOCK_PY="$TMP_ROOT/rtsp-mock.py"
cat >"$RTSP_MOCK_PY" <<'PY'
import re, socket, struct, sys

host, portfile, logfile, mode = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
client_port = int(sys.argv[5]) if len(sys.argv) > 5 else 0
server_port = int(sys.argv[6]) if len(sys.argv) > 6 else 0

sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, 0))
sock.listen(8)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))

log = open(logfile, "a", buffering=1)
read_bytes = 0


def reply(conn, text):
    conn.sendall(text.encode())


conn, peer = sock.accept()
log.write("accept %s:%s\n" % (peer[0], peer[1]))
conn.settimeout(15)
buf = b""
recorded = False
stalled = False
total = 0
session = "12345678"
try:
    while True:
        if recorded and buf[:1] == b"$":
            if len(buf) < 4:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                read_bytes += len(chunk)
                buf += chunk
                continue
            length = int.from_bytes(buf[2:4], "big")
            if len(buf) < 4 + length:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                read_bytes += len(chunk)
                buf += chunk
                continue
            channel = buf[1]
            total += length
            log.write("frame ch=%d len=%d total=%d\n" % (channel, length, total))
            buf = buf[4 + length:]
            if mode == "stall-after-media" and not stalled and total > 0:
                # Announce a large interleaved payload but never send the rest:
                # the sender's next write poll sees readable input and blocks in
                # ff_rtsp_skip_packet until the RTSP socket timeout.
                conn.sendall(b"$" + bytes([channel]) + struct.pack(">H", 65535) + b"\x00" * 8)
                log.write("stall_after_media total=%d\n" % total)
                stalled = True
            continue
        end = buf.find(b"\r\n\r\n")
        if end < 0:
            chunk = conn.recv(65536)
            if not chunk:
                break
            read_bytes += len(chunk)
            buf += chunk
            continue
        head, buf = buf[:end], buf[end + 4:]
        lines = head.decode("utf-8", "replace").split("\r\n")
        method = lines[0].split(" ")[0] if lines and lines[0] else ""
        headers = {}
        for line in lines[1:]:
            if ":" in line:
                key, value = line.split(":", 1)
                headers[key.strip().lower()] = value.strip()
        length = int(headers.get("content-length", "0"))
        while len(buf) < length:
            chunk = conn.recv(65536)
            if not chunk:
                break
            read_bytes += len(chunk)
            buf += chunk
        body, buf = buf[:length], buf[length:]
        cseq = headers.get("cseq", "0")
        if method == "OPTIONS":
            log.write("OPTIONS cseq=%s\n" % cseq)
            reply(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n"
                        "Public: OPTIONS, ANNOUNCE, SETUP, RECORD, TEARDOWN\r\n\r\n" % cseq)
        elif method == "ANNOUNCE":
            log.write("ANNOUNCE cseq=%s body_bytes=%d\n" % (cseq, len(body)))
            reply(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nSession: %s\r\n\r\n" % (cseq, session))
        elif method == "SETUP":
            requested = headers.get("transport", "")
            log.write("SETUP cseq=%s transport=%s\n" % (cseq, requested))
            wants_tcp = "TCP" in requested.upper()
            match = re.search(r"client_port=(\d+)", requested)
            cport = int(match.group(1)) if match else client_port
            transport = None
            if mode in ("tcp", "stall-after-media"):
                if wants_tcp:
                    transport = "RTP/AVP/TCP;unicast;interleaved=0-1;ssrc=1234ABCD"
            elif mode in ("udp", "udp-only", "udp-source"):
                if not wants_tcp:
                    transport = "RTP/AVP;unicast;client_port=%d;server_port=%d" % (cport, server_port)
                    if mode == "udp-source":
                        transport += ";source=127.0.0.1"
            if transport is None:
                log.write("SETUP-461 cseq=%s\n" % cseq)
                reply(conn, "RTSP/1.0 461 Unsupported Transport\r\nCSeq: %s\r\n\r\n" % cseq)
            else:
                log.write("SETUP-200 transport=%s\n" % transport)
                reply(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nTransport: %s\r\nSession: %s\r\n\r\n"
                      % (cseq, transport, session))
        elif method == "RECORD":
            log.write("RECORD cseq=%s\n" % cseq)
            reply(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nSession: %s\r\n\r\n" % (cseq, session))
            recorded = True
        elif method == "TEARDOWN":
            log.write("TEARDOWN cseq=%s\n" % cseq)
            reply(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nSession: %s\r\n\r\n" % (cseq, session))
            break
        else:
            log.write("OTHER %r\n" % head[:60])
            reply(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n\r\n" % cseq)
except (OSError, ValueError) as exc:
    log.write("error %s\n" % exc)
finally:
    log.write("done total=%d read_bytes=%d\n" % (total, read_bytes))
    try:
        conn.close()
    except OSError:
        pass
    sock.close()
PY

# start_rtsp_receiver runs one real RTSP receiver. mode "tcp" answers SETUP
# with interleaved RTP/RTCP on the control connection; "udp-source" answers
# with a UDP transport whose source is loopback; "stall-after-media" receives
# RTP and then announces a partial interleaved payload, so the sender blocks in
# ff_rtsp_skip_packet until its socket timeout.
start_rtsp_receiver() {
	local bind_ip="$1" portfile="$2" logfile="$3" mode="$4" client_port="$5" server_port="$6"
	: >"$logfile"
	python3 "$RTSP_MOCK_PY" "$bind_ip" "$portfile" "$logfile" "$mode" \
		"$client_port" "$server_port" >>"$TMP_ROOT/rtsp-mock.log" 2>&1 &
	echo "$!" >>"$PIDFILE"
}

# Real media over TCP interleaved: every RTP byte shares the judged control
# connection, and the receiver gets a complete RTSP session with TEARDOWN.
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-rtsp-tcp.test'
settle_hosts

RTSP_TCP_PORTFILE="$TMP_ROOT/rtsp-tcp.port"
RTSP_TCP_LOG="$TMP_ROOT/rtsp-tcp.log"
start_rtsp_receiver 10.77.0.2 "$RTSP_TCP_PORTFILE" "$RTSP_TCP_LOG" tcp 0 0
wait_portfile "$RTSP_TCP_PORTFILE"
RTSP_TCP_PORT="$(cat "$RTSP_TCP_PORTFILE")"

RTSP_TCP_TRAP_LOG="$TMP_ROOT/rtsp-tcp-trap.accepts"
RTSP_TCP_TRAP_PAYLOAD="$TMP_ROOT/rtsp-tcp-trap.bin"
: >"$RTSP_TCP_TRAP_LOG"
: >"$RTSP_TCP_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$RTSP_TCP_PORT" "$TMP_ROOT/rtsp-tcp-trap.port" \
	"$RTSP_TCP_TRAP_LOG" "$RTSP_TCP_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/rtsp-tcp-trap.port"

rc=0
run_cli "rtsp://tc15-rtsp-tcp.test:$RTSP_TCP_PORT/live" rtsp || rc=$?
expect_completed "RTSP interleaved push" "$rc"
deadline=$((SECONDS + 15))
until grep -q '^done ' "$RTSP_TCP_LOG"; do
	((SECONDS < deadline)) ||
		fail "RTSP interleaved push: the receiver never finished; log: $(cat "$RTSP_TCP_LOG")"
	sleep 0.2
done
RTSP_TCP_BYTES="$(sed -n 's/^done total=\([0-9]*\).*/\1/p' "$RTSP_TCP_LOG" | tail -1)"
[[ "${RTSP_TCP_BYTES:-0}" -gt 0 ]] ||
	fail "RTSP interleaved push: the judged receiver got no RTP bytes"
grep -q '^frame ch=0 ' "$RTSP_TCP_LOG" ||
	fail "RTSP interleaved push: no RTP frame arrived on the judged control connection"
grep -q '^TEARDOWN ' "$RTSP_TCP_LOG" ||
	fail "RTSP interleaved push: the receiver saw no TEARDOWN"
[[ "$(accept_count "$RTSP_TCP_TRAP_LOG")" == "0" ]] ||
	fail "RTSP interleaved push: a receiver outside the judged set was contacted"
echo "PASS: [rtsp-interleaved] control_accepts=$(accept_count "$RTSP_TCP_LOG") rtp_bytes=$RTSP_TCP_BYTES outside_accepts=$(accept_count "$RTSP_TCP_TRAP_LOG")"

# run_rtsp_udp_case exercises a UDP RTSP receiver. The judged address owns the
# RTP and RTCP ports; loopback counters at the same ports catch a SETUP source
# that was not rewritten. A TCP-only receiver answers 461 to the UDP SETUP so
# the default transport mask can fall back to interleaved TCP.
run_rtsp_udp_case() {
	local label="$1" mode="$2" expect_tcp_reject="$3"
	shift 3

	local rtp_port
	rtp_port="$(pick_udp_port 10.77.0.2)"
	while ((rtp_port >= 65535)); do
		rtp_port="$(pick_udp_port 10.77.0.2)"
	done
	local rtcp_port=$((rtp_port + 1))

	local portfile="$TMP_ROOT/$label.port"
	local logfile="$TMP_ROOT/$label.log"
	start_rtsp_receiver 10.77.0.2 "$portfile" "$logfile" "$mode" 0 "$rtp_port"
	wait_portfile "$portfile"
	local rtsp_port
	rtsp_port="$(cat "$portfile")"

	local judged_rtp_count="$TMP_ROOT/$label-judged-rtp.count"
	local judged_rtcp_count="$TMP_ROOT/$label-judged-rtcp.count"
	local trap_rtp_count="$TMP_ROOT/$label-trap-rtp.count"
	local trap_rtcp_count="$TMP_ROOT/$label-trap-rtcp.count"
	local outside_rtp_count="$TMP_ROOT/$label-outside-rtp.count"
	local outside_rtcp_count="$TMP_ROOT/$label-outside-rtcp.count"
	for count in "$judged_rtp_count" "$judged_rtcp_count" "$trap_rtp_count" "$trap_rtcp_count" \
		"$outside_rtp_count" "$outside_rtcp_count"; do
		echo 0 >"$count"
	done
	start_udp_counter 10.77.0.2 "$rtp_port" "$TMP_ROOT/$label-judged-rtp.datagrams" "$judged_rtp_count"
	start_udp_counter 10.77.0.2 "$rtcp_port" "$TMP_ROOT/$label-judged-rtcp.datagrams" "$judged_rtcp_count"
	start_udp_counter 127.0.0.1 "$rtp_port" "$TMP_ROOT/$label-trap-rtp.datagrams" "$trap_rtp_count"
	start_udp_counter 127.0.0.1 "$rtcp_port" "$TMP_ROOT/$label-trap-rtcp.datagrams" "$trap_rtcp_count"
	start_udp_counter 10.77.0.3 "$rtp_port" "$TMP_ROOT/$label-outside-rtp.datagrams" "$outside_rtp_count"
	start_udp_counter 10.77.0.3 "$rtcp_port" "$TMP_ROOT/$label-outside-rtcp.datagrams" "$outside_rtcp_count"

	local outside_tcp_log="$TMP_ROOT/$label-outside.accepts"
	local outside_tcp_payload="$TMP_ROOT/$label-outside.bin"
	: >"$outside_tcp_log"
	: >"$outside_tcp_payload"
	start_tcp_receiver 10.77.0.3 "$rtsp_port" "$TMP_ROOT/$label-outside.port" \
		"$outside_tcp_log" "$outside_tcp_payload"
	wait_portfile "$TMP_ROOT/$label-outside.port"
	sleep 0.3

	rc=0
	run_cli "rtsp://tc15-rtsp-udp.test:$rtsp_port/live" rtsp "$@" || rc=$?
	expect_completed "$label" "$rc"

	local deadline=$((SECONDS + 15))
	until [[ "$(cat "$judged_rtp_count")" -gt 0 ]]; do
		((SECONDS < deadline)) ||
			fail "$label: judged RTP port got no datagrams; log: $(cat "$logfile")"
		sleep 0.2
	done
	sleep 0.5
	[[ "$(cat "$trap_rtp_count")" == "0" ]] ||
		fail "$label: RTP reached the loopback trap"
	[[ "$(cat "$trap_rtcp_count")" == "0" ]] ||
		fail "$label: RTCP reached the loopback trap"
	[[ "$(cat "$outside_rtp_count")" == "0" ]] ||
		fail "$label: RTP reached a receiver outside the judged set"
	[[ "$(cat "$outside_rtcp_count")" == "0" ]] ||
		fail "$label: RTCP reached a receiver outside the judged set"
	[[ "$(accept_count "$outside_tcp_log")" == "0" ]] ||
		fail "$label: a TCP receiver outside the judged set was contacted"
	if [[ "$expect_tcp_reject" == "1" ]]; then
		grep -q '^SETUP-461 ' "$logfile" ||
			fail "$label: the UDP-only receiver did not reject a TCP SETUP"
	fi
	echo "PASS: [$label] judged_rtp_datagrams=$(cat "$judged_rtp_count") judged_rtcp_datagrams=$(cat "$judged_rtcp_count") loopback_rtp=0 loopback_rtcp=0 outside_datagrams=0 outside_accepts=$(accept_count "$outside_tcp_log")"
}

set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-rtsp-udp.test'
settle_hosts

# Explicit UDP must not be overridden: the receiver rejects TCP SETUP with 461
# and accepts the UDP transport, and its RTP lands on the judged address.
run_rtsp_udp_case rtsp-udp-explicit udp-only 0 -rtsp_transport udp

# The same UDP-only receiver must work through ffmpeg's default transport mask,
# which tries UDP first and only falls back to TCP on 461.
run_rtsp_udp_case rtsp-udp-default udp-only 0

# A SETUP source=127.0.0.1 is a server-chosen media destination. The relay
# rewrites it to the judged IP, so RTP/RTCP still land there and the loopback
# trap stays at zero.
run_rtsp_udp_case rtsp-udp-source udp-source 0 -rtsp_transport udp

# Audio-only RTSP with negative output timestamps (-itsoffset before the input,
# -copyts, negative-ts shifting disabled): the receiver gets real RTP media and
# then announces a partial interleaved payload, so ffmpeg's next write poll
# blocks in ff_rtsp_skip_packet until the RTSP socket timeout (exit 146,
# result.IsTimeout=false). The old progress inference saw only non-positive
# out_time_us here and re-pushed; the worker's own established dial must stop
# the attempt set instead, leaving the second judged address untouched.
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-rtsp-stall.test' '10.77.0.2 tc15-rtsp-stall.test'
settle_hosts

RTSP_STALL_AUDIO="$TMP_ROOT/audio-stall.m4a"
ffmpeg -hide_banner -loglevel error -y -f lavfi -i "sine=frequency=440:duration=5" \
	-c:a aac "$RTSP_STALL_AUDIO"

RTSP_STALL_PORTFILE="$TMP_ROOT/rtsp-stall.port"
RTSP_STALL_LOG="$TMP_ROOT/rtsp-stall.log"
start_rtsp_receiver 10.77.0.1 "$RTSP_STALL_PORTFILE" "$RTSP_STALL_LOG" stall-after-media 0 0
wait_portfile "$RTSP_STALL_PORTFILE"
RTSP_STALL_PORT="$(cat "$RTSP_STALL_PORTFILE")"

RTSP_STALL_SECOND_LOG="$TMP_ROOT/rtsp-stall-second.accepts"
RTSP_STALL_SECOND_PAYLOAD="$TMP_ROOT/rtsp-stall-second.bin"
: >"$RTSP_STALL_SECOND_LOG"
: >"$RTSP_STALL_SECOND_PAYLOAD"
start_tcp_receiver 10.77.0.2 "$RTSP_STALL_PORT" "$TMP_ROOT/rtsp-stall-second.port" \
	"$RTSP_STALL_SECOND_LOG" "$RTSP_STALL_SECOND_PAYLOAD"
wait_portfile "$TMP_ROOT/rtsp-stall-second.port"

RTSP_STALL_TRAP_LOG="$TMP_ROOT/rtsp-stall-trap.accepts"
RTSP_STALL_TRAP_PAYLOAD="$TMP_ROOT/rtsp-stall-trap.bin"
: >"$RTSP_STALL_TRAP_LOG"
: >"$RTSP_STALL_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$RTSP_STALL_PORT" "$TMP_ROOT/rtsp-stall-trap.port" \
	"$RTSP_STALL_TRAP_LOG" "$RTSP_STALL_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/rtsp-stall-trap.port"

CLI_LOG="$TMP_ROOT/cli-rtsp-stall.log"
set +e
"$RFFMPEG_BIN/rffmpeg" -itsoffset -3600 -i "$RTSP_STALL_AUDIO" \
	-vn -c:a copy -t 5 -copyts -avoid_negative_ts disabled -timeout 200000 -f rtsp \
	"rtsp://tc15-rtsp-stall.test:$RTSP_STALL_PORT/live" 2>"$CLI_LOG"
rc=$?
set -e
[[ $rc -eq 1 ]] || fail "RTSP audio stall: rc=$rc, want 1; see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "RTSP audio stall: no job id in the CLI log"
[[ "$(job_field "$JOB" status)" == "failed" ]] ||
	fail "RTSP audio stall: job status $(job_field "$JOB" status), want failed"
grep -qF 'Connection timed out' "$CLI_LOG" ||
	fail "RTSP audio stall: ffmpeg did not fail through the RTSP socket timeout; see $CLI_LOG"
RTSP_STALL_BYTES="$(sed -n 's/^stall_after_media total=\([0-9]*\).*/\1/p' "$RTSP_STALL_LOG" | tail -1)"
[[ "${RTSP_STALL_BYTES:-0}" -gt 0 ]] ||
	fail "RTSP audio stall: the first judged receiver got no media before the stall"
[[ "$(accept_count "$RTSP_STALL_SECOND_LOG")" == "0" ]] ||
	fail "RTSP audio stall: the second judged address received a duplicate connection"
[[ "$(payload_size "$RTSP_STALL_SECOND_PAYLOAD")" == "0" ]] ||
	fail "RTSP audio stall: the second judged address received duplicate media"
[[ "$(accept_count "$RTSP_STALL_TRAP_LOG")" == "0" ]] ||
	fail "RTSP audio stall: a receiver outside the judged set was contacted"
echo "PASS: [rtsp-audio-stall] input_offset=-3600s first_media_bytes=$RTSP_STALL_BYTES second_accepts=$(accept_count "$RTSP_STALL_SECOND_LOG") second_bytes=$(payload_size "$RTSP_STALL_SECOND_PAYLOAD") outside_accepts=$(accept_count "$RTSP_STALL_TRAP_LOG")"

# ------------------------------------------------------------ RTP/RTCP 双通道
# FFmpeg's RTP muxer opens two independent UDP sockets: media to the URL port
# and RTCP to the explicit rtcpport or the URL port+1. A single relay cannot
# represent it. Each case runs real receivers on the judged address and traps
# on loopback at the same ports; -rtpflags send_bye forces a real RTCP packet.
run_rtp_case() {
	local label="$1" rtp_port="$2" rtcp_port="$3" query="$4"
	shift 4

	local judged_rtp="$TMP_ROOT/$label-judged-rtp.count"
	local judged_rtcp="$TMP_ROOT/$label-judged-rtcp.count"
	local trap_rtp="$TMP_ROOT/$label-trap-rtp.count"
	local trap_rtcp="$TMP_ROOT/$label-trap-rtcp.count"
	local outside_rtp="$TMP_ROOT/$label-outside-rtp.count"
	local outside_rtcp="$TMP_ROOT/$label-outside-rtcp.count"
	for count in "$judged_rtp" "$judged_rtcp" "$trap_rtp" "$trap_rtcp" "$outside_rtp" "$outside_rtcp"; do
		echo 0 >"$count"
	done
	start_udp_counter 10.77.0.2 "$rtp_port" "$TMP_ROOT/$label-judged-rtp.datagrams" "$judged_rtp"
	start_udp_counter 10.77.0.2 "$rtcp_port" "$TMP_ROOT/$label-judged-rtcp.datagrams" "$judged_rtcp"
	start_udp_counter 127.0.0.1 "$rtp_port" "$TMP_ROOT/$label-trap-rtp.datagrams" "$trap_rtp"
	start_udp_counter 127.0.0.1 "$rtcp_port" "$TMP_ROOT/$label-trap-rtcp.datagrams" "$trap_rtcp"
	start_udp_counter 10.77.0.3 "$rtp_port" "$TMP_ROOT/$label-outside-rtp.datagrams" "$outside_rtp"
	start_udp_counter 10.77.0.3 "$rtcp_port" "$TMP_ROOT/$label-outside-rtcp.datagrams" "$outside_rtcp"
	sleep 0.3

	local target="rtp://tc15-rtp.test:$rtp_port"
	if [[ -n "$query" ]]; then
		target="$target?$query"
	fi
	rc=0
	run_cli "$target" rtp -rtpflags send_bye "$@" || rc=$?
	expect_completed "$label" "$rc"

	local deadline=$((SECONDS + 15))
	until [[ "$(cat "$judged_rtp")" -gt 0 ]]; do
		((SECONDS < deadline)) || fail "$label: judged RTP port got no datagrams"
		sleep 0.2
	done
	until [[ "$(cat "$judged_rtcp")" -gt 0 ]]; do
		((SECONDS < deadline)) || fail "$label: judged RTCP port got no datagrams"
		sleep 0.2
	done
	sleep 0.5
	[[ "$(cat "$trap_rtp")" == "0" ]] || fail "$label: RTP reached the loopback trap"
	[[ "$(cat "$trap_rtcp")" == "0" ]] || fail "$label: RTCP reached the loopback trap"
	[[ "$(cat "$outside_rtp")" == "0" ]] || fail "$label: RTP reached a receiver outside the judged set"
	[[ "$(cat "$outside_rtcp")" == "0" ]] || fail "$label: RTCP reached a receiver outside the judged set"
	echo "PASS: [$label] judged_rtp_datagrams=$(cat "$judged_rtp") judged_rtcp_datagrams=$(cat "$judged_rtcp") loopback_rtp=0 loopback_rtcp=0 outside_datagrams=0"
}

set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-rtp.test'
settle_hosts

RTP_DEFAULT_PORT="$(pick_udp_port 10.77.0.2)"
while ((RTP_DEFAULT_PORT >= 65535)); do
	RTP_DEFAULT_PORT="$(pick_udp_port 10.77.0.2)"
done
run_rtp_case rtp-default-rtcp "$RTP_DEFAULT_PORT" "$((RTP_DEFAULT_PORT + 1))" ""

RTP_EXPLICIT_PORT="$(pick_udp_port 10.77.0.2)"
RTP_EXPLICIT_RTCP_PORT="$(pick_udp_port 10.77.0.2)"
while ((RTP_EXPLICIT_RTCP_PORT == RTP_EXPLICIT_PORT)); do
	RTP_EXPLICIT_RTCP_PORT="$(pick_udp_port 10.77.0.2)"
done
run_rtp_case rtp-explicit-rtcp "$RTP_EXPLICIT_PORT" "$RTP_EXPLICIT_RTCP_PORT" \
	"rtcpport=$RTP_EXPLICIT_RTCP_PORT"

# ------------------------------------------------------------ HTTPS 名字语义
# TLS carries its own name: the CONNECT target must reach the judged address
# while the URL host stays the request's name, so SNI and certificate
# verification use that name. An address-pinned URL would send SNI for the IP
# and fail the certificate check.
command -v openssl >/dev/null 2>&1 ||
	fail "HTTPS name semantics needs openssl to create the receiver certificate"

set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-https.test'
settle_hosts

TLS_CERT="$TMP_ROOT/tc15-https.crt"
TLS_KEY="$TMP_ROOT/tc15-https.key"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$TLS_KEY" -out "$TLS_CERT" \
	-subj /CN=tc15-https.test -addext subjectAltName=DNS:tc15-https.test -days 1 \
	>>"$TMP_ROOT/openssl.log" 2>&1 ||
	fail "HTTPS name semantics: could not create the receiver certificate; see $TMP_ROOT/openssl.log"

# The CLI refuses absolute host paths in job arguments, so the receiver's
# certificate joins the namespace's system trust store instead of travelling
# as -ca_file. Verification stays on and still checks the request host name.
# GnuTLS reads p11-kit anchors on this distribution; the bundle file is the
# fallback for distributions that expose only the OpenSSL trust bundle.
TLS_TRUST_ANCHORS=/etc/ca-certificates/trust-source/anchors
if [[ -d "$TLS_TRUST_ANCHORS" ]]; then
	TLS_ANCHOR_COPY="$TMP_ROOT/tc15-https-anchors"
	mkdir -p "$TLS_ANCHOR_COPY"
	cp -R "$TLS_TRUST_ANCHORS/." "$TLS_ANCHOR_COPY/"
	cp "$TLS_CERT" "$TLS_ANCHOR_COPY/tc15-https.crt"
	mount --bind "$TLS_ANCHOR_COPY" "$TLS_TRUST_ANCHORS"
else
	TLS_CA_BUNDLE="$TMP_ROOT/tc15-https-ca-bundle.crt"
	cat /etc/ssl/certs/ca-certificates.crt "$TLS_CERT" >"$TLS_CA_BUNDLE"
	mount --bind "$TLS_CA_BUNDLE" /etc/ssl/certs/ca-certificates.crt
fi

HTTPS_RECEIVER_PY="$TMP_ROOT/https-receiver.py"
cat >"$HTTPS_RECEIVER_PY" <<'PY'
import http.server, ssl, sys

host, portfile, logfile, cert, key = sys.argv[1:6]
log = open(logfile, "a", buffering=1)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)


def sni(sslobj, name, _ctx):
    log.write("sni=%r\n" % name)


ctx.sni_callback = sni


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def handle_body(self):
        total = 0
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            while True:
                line = self.rfile.readline()
                if not line:
                    break
                size = int(line.strip().split(b";")[0], 16)
                if size == 0:
                    self.rfile.readline()
                    break
                total += len(self.rfile.read(size))
                self.rfile.read(2)
        else:
            total = len(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        log.write("request=%s %s host=%s body_bytes=%d\n" %
                  (self.command, self.path, self.headers.get("Host"), total))
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_POST = handle_body
    do_PUT = handle_body


httpd = http.server.ThreadingHTTPServer((host, 0), Handler)
with open(portfile, "w") as f:
    f.write(str(httpd.server_address[1]))
httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
httpd.serve_forever()
PY

HTTPS_PORTFILE="$TMP_ROOT/https.port"
HTTPS_LOG="$TMP_ROOT/https.log"
: >"$HTTPS_LOG"
python3 "$HTTPS_RECEIVER_PY" 10.77.0.2 "$HTTPS_PORTFILE" "$HTTPS_LOG" \
	"$TLS_CERT" "$TLS_KEY" >>"$TMP_ROOT/https-server.log" 2>&1 &
echo "$!" >>"$PIDFILE"
wait_portfile "$HTTPS_PORTFILE"
HTTPS_PORT="$(cat "$HTTPS_PORTFILE")"

HTTPS_TRAP_LOG="$TMP_ROOT/https-trap.accepts"
HTTPS_TRAP_PAYLOAD="$TMP_ROOT/https-trap.bin"
: >"$HTTPS_TRAP_LOG"
: >"$HTTPS_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$HTTPS_PORT" "$TMP_ROOT/https-trap.port" \
	"$HTTPS_TRAP_LOG" "$HTTPS_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/https-trap.port"

rc=0
run_cli "https://tc15-https.test:$HTTPS_PORT/live" mpegts || rc=$?
expect_completed "HTTPS name semantics" "$rc"
deadline=$((SECONDS + 15))
until grep -q '^request=' "$HTTPS_LOG"; do
	((SECONDS < deadline)) ||
		fail "HTTPS name semantics: the receiver saw no request; log: $(cat "$HTTPS_LOG")"
	sleep 0.2
done
grep -qxF "sni='tc15-https.test'" "$HTTPS_LOG" ||
	fail "HTTPS name semantics: the receiver saw SNI $(sed -n "s/^sni=//p" "$HTTPS_LOG"), want the request host"
HTTPS_BODY_BYTES="$(sed -n 's/^request=.* body_bytes=\([0-9]*\)$/\1/p' "$HTTPS_LOG" | tail -1)"
[[ "${HTTPS_BODY_BYTES:-0}" -gt 0 ]] ||
	fail "HTTPS name semantics: the judged receiver got no media body"
grep -qF "host=tc15-https.test:$HTTPS_PORT" "$HTTPS_LOG" ||
	fail "HTTPS name semantics: the receiver saw Host $(sed -n 's/^.*host=//p' "$HTTPS_LOG"), want the request host"
[[ "$(accept_count "$HTTPS_TRAP_LOG")" == "0" ]] ||
	fail "HTTPS name semantics: a receiver outside the judged set was contacted"
echo "PASS: [https-name] sni=tc15-https.test body_bytes=$HTTPS_BODY_BYTES outside_accepts=$(accept_count "$HTTPS_TRAP_LOG")"

# ------------------------------------------------ RTMP 会话名字保留
# The socket must land on the judged IP while the RTMP connect message keeps the
# request's tcUrl host name.
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-rtmp.test'
settle_hosts

RTMP_CAPTURE_PY="$TMP_ROOT/rtmp-capture.py"
cat >"$RTMP_CAPTURE_PY" <<'PY'
import os, socket, sys

host, portfile, outdir = sys.argv[1], sys.argv[2], sys.argv[3]
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, 0))
sock.listen(8)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))


def recvn(conn, n):
    data = b""
    while len(data) < n:
        chunk = conn.recv(n - len(data))
        if not chunk:
            raise EOFError
        data += chunk
    return data


def read_message(conn, bufs, chunk_size):
    first = recvn(conn, 1)[0]
    fmt = first >> 6
    csid = first & 0x3f
    if csid == 0:
        csid = 64 + recvn(conn, 1)[0]
    elif csid == 1:
        raw = recvn(conn, 2)
        csid = 64 + raw[0] + raw[1] * 256
    state = bufs.setdefault(csid, {"length": 0, "type": 0, "payload": bytearray()})
    if fmt <= 2:
        timestamp = int.from_bytes(recvn(conn, 3), "big")
    else:
        timestamp = 0
    if fmt <= 1:
        state["length"] = int.from_bytes(recvn(conn, 3), "big")
        state["type"] = recvn(conn, 1)[0]
        if fmt == 0:
            recvn(conn, 4)
    if timestamp == 0xFFFFFF:
        recvn(conn, 4)
    remaining = state["length"] - len(state["payload"])
    state["payload"] += recvn(conn, min(chunk_size, remaining))
    if len(state["payload"]) < state["length"]:
        return None
    message = (state["type"], bytes(state["payload"]))
    bufs[csid] = {"length": 0, "type": 0, "payload": bytearray()}
    return message


def amf0_string(payload, key):
    # AMF0 object property names are a 2-byte length and raw UTF-8; only the
    # value carries a type marker (0x02 for a string).
    needle = len(key).to_bytes(2, "big") + key.encode("utf-8") + b"\x02"
    position = payload.find(needle)
    if position < 0:
        return None
    value_at = position + len(needle)
    if value_at + 2 > len(payload):
        return None
    length = int.from_bytes(payload[value_at:value_at + 2], "big")
    if value_at + 2 + length > len(payload):
        return None
    return payload[value_at + 2:value_at + 2 + length].decode("utf-8", "replace")


while True:
    conn, _ = sock.accept()
    conn.settimeout(10)
    try:
        recvn(conn, 1)
        c1 = recvn(conn, 1536)
    except (EOFError, OSError):
        conn.close()
        continue
    conn.sendall(b"\x03" + b"\x00" * 1536 + c1)
    try:
        recvn(conn, 1536)
        bufs = {}
        while True:
            message = read_message(conn, bufs, 128)
            if message is None:
                continue
            msg_type, payload = message
            if msg_type != 20:
                continue
            tc_url = amf0_string(payload, "tcUrl")
            if tc_url is None:
                continue
            app = amf0_string(payload, "app")
            if app is not None:
                with open(os.path.join(outdir, "app"), "w") as f:
                    f.write(app)
            with open(os.path.join(outdir, "tcUrl"), "w") as f:
                f.write(tc_url)
            conn.close()
            sock.close()
            sys.exit(0)
    except (EOFError, OSError, ValueError):
        conn.close()
        continue
PY

# start_rtmp_capture runs the AMF0 connect-message probe. It writes the app and
# tcUrl fields it received to <fields>/app and <fields>/tcUrl.
start_rtmp_capture() {
	local bind_ip="$1" portfile="$2" fields="$3" logfile="$4"
	rm -rf "$fields"
	mkdir -p "$fields"
	python3 "$RTMP_CAPTURE_PY" "$bind_ip" "$portfile" "$fields" >>"$logfile" 2>&1 &
	echo "$!" >>"$PIDFILE"
}

RTMP_PORTFILE="$TMP_ROOT/rtmp.port"
RTMP_FIELDS="$TMP_ROOT/rtmp-fields"
start_rtmp_capture 10.77.0.2 "$RTMP_PORTFILE" "$RTMP_FIELDS" "$TMP_ROOT/rtmp-capture.log"
wait_portfile "$RTMP_PORTFILE"
RTMP_PORT="$(cat "$RTMP_PORTFILE")"

RTMP_TRAP_LOG="$TMP_ROOT/rtmp-trap.accepts"
RTMP_TRAP_PAYLOAD="$TMP_ROOT/rtmp-trap.bin"
: >"$RTMP_TRAP_LOG"
: >"$RTMP_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$RTMP_PORT" "$TMP_ROOT/rtmp-trap.port" "$RTMP_TRAP_LOG" "$RTMP_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/rtmp-trap.port"

rc=0
run_cli "rtmp://tc15-rtmp.test:$RTMP_PORT/live/key" flv || rc=$?
[[ $rc -eq 1 ]] || fail "RTMP tcUrl push: rc=$rc, want 1; see $CLI_LOG"
deadline=$((SECONDS + 10))
until [[ -s "$RTMP_FIELDS/tcUrl" ]]; do
	((SECONDS < deadline)) || fail "RTMP tcUrl push: the receiver captured no connect message"
	sleep 0.2
done
EXPECTED_TCURL="rtmp://tc15-rtmp.test:$RTMP_PORT/live"
[[ "$(cat "$RTMP_FIELDS/tcUrl")" == "$EXPECTED_TCURL" ]] ||
	fail "RTMP tcUrl push: tcUrl $(cat "$RTMP_FIELDS/tcUrl"), want $EXPECTED_TCURL"
[[ "$(cat "$RTMP_FIELDS/app" 2>/dev/null)" == "live" ]] ||
	fail "RTMP tcUrl push: app $(cat "$RTMP_FIELDS/app" 2>/dev/null), want live"
[[ "$(accept_count "$RTMP_TRAP_LOG")" == "0" ]] ||
	fail "RTMP tcUrl push: a receiver outside the judged set was contacted"
echo "PASS: [rtmp-tcurl] app=$(cat "$RTMP_FIELDS/app") tcUrl=$(cat "$RTMP_FIELDS/tcUrl") outside_accepts=$(accept_count "$RTMP_TRAP_LOG")"

# --------------------------------------------- RTMP 应用实例路径保留
# The native app/playpath split keeps an instance segment in the app:
# /live/instance/key belongs to app live/instance, so the default tcUrl must
# carry that whole app while the socket still lands on the judged address.
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-rtmp-inst.test'
settle_hosts

RTMP_INST_PORTFILE="$TMP_ROOT/rtmp-inst.port"
RTMP_INST_FIELDS="$TMP_ROOT/rtmp-inst-fields"
start_rtmp_capture 10.77.0.2 "$RTMP_INST_PORTFILE" "$RTMP_INST_FIELDS" "$TMP_ROOT/rtmp-inst-capture.log"
wait_portfile "$RTMP_INST_PORTFILE"
RTMP_INST_PORT="$(cat "$RTMP_INST_PORTFILE")"

RTMP_INST_TRAP_LOG="$TMP_ROOT/rtmp-inst-trap.accepts"
RTMP_INST_TRAP_PAYLOAD="$TMP_ROOT/rtmp-inst-trap.bin"
: >"$RTMP_INST_TRAP_LOG"
: >"$RTMP_INST_TRAP_PAYLOAD"
start_tcp_receiver 10.77.0.3 "$RTMP_INST_PORT" "$TMP_ROOT/rtmp-inst-trap.port" \
	"$RTMP_INST_TRAP_LOG" "$RTMP_INST_TRAP_PAYLOAD"
wait_portfile "$TMP_ROOT/rtmp-inst-trap.port"

rc=0
run_cli "rtmp://tc15-rtmp-inst.test:$RTMP_INST_PORT/live/instance/key" flv || rc=$?
[[ $rc -eq 1 ]] || fail "RTMP instance push: rc=$rc, want 1; see $CLI_LOG"
deadline=$((SECONDS + 10))
until [[ -s "$RTMP_INST_FIELDS/tcUrl" ]]; do
	((SECONDS < deadline)) || fail "RTMP instance push: the receiver captured no connect message"
	sleep 0.2
done
EXPECTED_APP="live/instance"
EXPECTED_TCURL="rtmp://tc15-rtmp-inst.test:$RTMP_INST_PORT/$EXPECTED_APP"
[[ "$(cat "$RTMP_INST_FIELDS/app")" == "$EXPECTED_APP" ]] ||
	fail "RTMP instance push: app $(cat "$RTMP_INST_FIELDS/app"), want $EXPECTED_APP"
[[ "$(cat "$RTMP_INST_FIELDS/tcUrl")" == "$EXPECTED_TCURL" ]] ||
	fail "RTMP instance push: tcUrl $(cat "$RTMP_INST_FIELDS/tcUrl"), want $EXPECTED_TCURL"
[[ "$(accept_count "$RTMP_INST_TRAP_LOG")" == "0" ]] ||
	fail "RTMP instance push: a receiver outside the judged set was contacted"
echo "PASS: [rtmp-app-instance] app=$(cat "$RTMP_INST_FIELDS/app") tcUrl=$(cat "$RTMP_INST_FIELDS/tcUrl") outside_accepts=$(accept_count "$RTMP_INST_TRAP_LOG")"

# ---------------------------------------- CONNECT 隧道随作业关闭（真实 socket）
# A local client and receiver both keep the tunnel open without sending. The
# proxy Close() must close both ends; the Go case uses real sockets and checks
# that neither side survives the job.
CONNECT_TEST_OUTPUT="$(cd "$REPO_ROOT" && go test ./pkg/worker -run '^TestJudgedHTTPProxyCloseClosesHeldConnectTunnel$' -count=1 -v 2>&1)" ||
	fail "CONNECT close: the real-socket regression failed: $CONNECT_TEST_OUTPUT"
grep -q -- '--- PASS: TestJudgedHTTPProxyCloseClosesHeldConnectTunnel' <<<"$CONNECT_TEST_OUTPUT" ||
	fail "CONNECT close: the regression did not pass: $CONNECT_TEST_OUTPUT"
echo "PASS: [connect-close] $(grep -o 'upstream_accepts=1 tunnel_bytes=0 client_closed=true upstream_closed=true' <<<"$CONNECT_TEST_OUTPUT" | tail -1)"

echo "PASS: a post-judgement rebind to a denied receiver leaves the connection on the judged address; address selection uses the worker's own relay/proxy dial observations (attempted and established), never ffmpeg's exit status, progress stream or log text, so a two-address judgement completes on the reachable judged address (including with -loglevel quiet from a temp directory containing metacharacters, and when a 5.1-style generic exit code replaces the connect errno) and an all-unreachable judged set fails FFMPEG_ERROR, all with zero outside connections; media already delivered to a resetting first address is never duplicated to the second; a native single-connection receiver gets the real stream on its only accept; RTSP honors explicit and default UDP with the SETUP source pinned to the judged IP (zero loopback datagrams) and still carries interleaved RTP over the judged TCP connection; RTP and RTCP use separate judged relays for both the default and an explicit rtcpport; an audio-only negative-timestamp RTSP whose receiver stalls after delivering media is never repeated on the second judged address; HTTPS keeps the request host for SNI, certificate verification and HTTP Host while the judged proxy takes the socket; an HTTP 307 and an RTMPT tunnel 307 to a loopback receiver are refused with zero trap accepts; RTMP keeps the requested host and app instance in tcUrl, RTMPT keeps the request host in HTTP Host, and a held CONNECT tunnel is closed on both ends when the job ends"
