#!/usr/bin/env bash
# TC-security-15: 判定地址即连接地址：改指向与多地址落点（AC-61）
#
# 可观察判据（docs/03-acceptance.md AC-61）:
#   - 判定后把目标主机名改指向拒绝类接收端 → 放行侧收到数据、拒绝类侧零连接，
#     作业结果与不改指向时相同（completed，CLI rc=0）。
#   - 判定得到两个放行地址、只有第二个可达 → 作业在第二个地址完成，清单中第一个
#     为 `unreachable`、第二个为 `connected`；判定集合之外的接收端零连接。
#   - 判定所得的地址全部不可达 → 作业失败 FFMPEG_ERROR，判定集合之外的接收端
#     零连接。
#
# 判据只取该作业的连接清单（C-134）与各接收端的接收记录，不看 ffmpeg 的退出码、
# 日志或进度。推流目标用表内的 `rtmp` 会话族（C-141）。
#
# 验收边界：「判定之后改指向」需要一个验收方可控的解析入口。本脚本在
# user+mount+network namespace 里运行：把 10.77.0.1–.3 挂到 lo，并把一份
# 临时 /etc/hosts bind mount 到系统路径，解析记录与接收端都由本用例控制，
# 不需要宿主 root，也不改动宿主 /etc/hosts。
#
# 旧版脚本里的 HTTP/RTMPT 重定向、SRT、RTSP、HTTPS 等场景属 AC-62/AC-63/AC-64
# （TC-security-16～18），不在本用例范围内。

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

# Go's resolver keeps a short cache. Wait one refresh window after rewriting
# /etc/hosts so the next judgement reads the controlled entries.
settle_hosts() {
	sleep 6
}

wait_portfile() {
	local portfile="$1" deadline=$((SECONDS + 10))
	until [[ -s "$portfile" ]]; do
		((SECONDS < deadline)) || fail "receiver did not start: $portfile"
		sleep 0.1
	done
}

accept_count() {
	grep -c '^accept ' "$1" 2>/dev/null || true
}

media_bytes() {
	cat "$1" 2>/dev/null || echo 0
}

# --------------------------------------------------------------- receivers
# start_tcp_counter records every accepted connection. It is the zero-connection
# observation for a receiver outside the judged set: any landing is counted.
start_tcp_counter() {
	local bind_ip="$1" port="$2" portfile="$3" logfile="$4"
	python3 - "$bind_ip" "$port" "$portfile" "$logfile" <<'PY' &
import socket, sys

host, port, portfile, logfile = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((host, port))
sock.listen(8)
with open(portfile, "w") as f:
    f.write(str(sock.getsockname()[1]))
with open(logfile, "a", buffering=1) as log:
    while True:
        conn, peer = sock.accept()
        log.write("accept %s:%s\n" % peer)
        conn.close()
PY
	echo "$!" >>"$PIDFILE"
}

# start_rtmp_receiver is a real RTMP publish endpoint: it completes the
# handshake, answers connect / createStream / publish, and counts the media
# message bytes it received. The byte count is the proof that the judged
# address really carried the stream, not just that a socket opened.
start_rtmp_receiver() {
	local bind_ip="$1" portfile="$2" logfile="$3" mediafile="$4"
	python3 - "$bind_ip" "$portfile" "$logfile" "$mediafile" <<'PY' &
import os, socket, struct, sys, threading

def recvn(conn, n):
    data = b""
    while len(data) < n:
        chunk = conn.recv(n - len(data))
        if not chunk:
            raise EOFError
        data += chunk
    return data

def amf_read(buf, i):
    kind = buf[i]
    i += 1
    if kind == 0x00:
        return struct.unpack(">d", buf[i:i + 8])[0], i + 8
    if kind == 0x01:
        return buf[i] != 0, i + 1
    if kind == 0x02:
        size = int.from_bytes(buf[i:i + 2], "big")
        return buf[i + 2:i + 2 + size].decode("utf-8", "replace"), i + 2 + size
    if kind in (0x03, 0x08):
        if kind == 0x08:
            i += 4
        obj = {}
        while True:
            size = int.from_bytes(buf[i:i + 2], "big")
            i += 2
            if size == 0:
                return obj, i + 1
            key = buf[i:i + size].decode("utf-8", "replace")
            i += size
            value, i = amf_read(buf, i)
            obj[key] = value
    if kind == 0x0A:
        count = int.from_bytes(buf[i:i + 4], "big")
        i += 4
        items = []
        for _ in range(count):
            value, i = amf_read(buf, i)
            items.append(value)
        return items, i
    if kind in (0x05, 0x06):
        return None, i
    raise ValueError("amf0 type %d" % kind)

def amf_str(text):
    raw = text.encode()
    return b"\x02" + len(raw).to_bytes(2, "big") + raw

def amf_num(value):
    return b"\x00" + struct.pack(">d", value)

def amf_object(pairs):
    out = b"\x03"
    for key, value in pairs.items():
        raw = key.encode()
        out += len(raw).to_bytes(2, "big") + raw + value
    return out + b"\x00\x00\x09"

def send_message(conn, csid, stream_id, msg_type, payload):
    out = bytearray([csid])
    out += b"\x00\x00\x00"
    out += len(payload).to_bytes(3, "big")
    out.append(msg_type)
    out += stream_id.to_bytes(4, "little")
    out += payload[:128]
    rest = payload[128:]
    while rest:
        out += bytes([0xC0 | csid]) + rest[:128]
        rest = rest[128:]
    conn.sendall(bytes(out))

class Reader:
    def __init__(self, conn):
        self.conn = conn
        self.state = {}
        self.chunk = 128

    def message(self):
        while True:
            first = recvn(self.conn, 1)[0]
            fmt = first >> 6
            csid = first & 0x3f
            if csid == 0:
                csid = 64 + recvn(self.conn, 1)[0]
            elif csid == 1:
                raw = recvn(self.conn, 2)
                csid = 64 + raw[0] + raw[1] * 256
            state = self.state.setdefault(csid, {"length": 0, "type": 0, "stream": 0, "ts": 0, "payload": bytearray()})
            if fmt <= 2:
                ts = int.from_bytes(recvn(self.conn, 3), "big")
            else:
                ts = state["ts"]
            if fmt <= 1:
                state["length"] = int.from_bytes(recvn(self.conn, 3), "big")
                state["type"] = recvn(self.conn, 1)[0]
                if fmt == 0:
                    state["stream"] = int.from_bytes(recvn(self.conn, 4), "little")
            if ts == 0xFFFFFF:
                ts = int.from_bytes(recvn(self.conn, 4), "big")
            state["ts"] = ts
            remaining = state["length"] - len(state["payload"])
            state["payload"] += recvn(self.conn, min(self.chunk, remaining))
            if len(state["payload"]) < state["length"]:
                continue
            message = (state["type"], state["stream"], bytes(state["payload"]))
            state["payload"] = bytearray()
            if message[0] == 1 and len(message[2]) >= 4:
                self.chunk = int.from_bytes(message[2][:4], "big") & 0x7FFFFFFF
            return message

lock = threading.Lock()
log = open(sys.argv[3], "a", buffering=1)
media_total = 0

def record_media(size):
    global media_total
    with lock:
        media_total += size
        with open(sys.argv[4], "w") as f:
            f.write(str(media_total))

def serve(conn):
    try:
        conn.settimeout(30)
        recvn(conn, 1)
        c1 = recvn(conn, 1536)
        s1 = b"\x00" * 8 + os.urandom(1528)
        conn.sendall(b"\x03" + s1 + c1)
        recvn(conn, 1536)
        reader = Reader(conn)
        while True:
            msg_type, stream_id, payload = reader.message()
            if msg_type != 20:
                if msg_type in (8, 9, 18):
                    record_media(len(payload))
                continue
            name, at = amf_read(payload, 0)
            txn, at = amf_read(payload, at)
            if name == "connect":
                body = (amf_str("_result") + amf_num(txn) +
                        amf_object({"fmsVer": amf_str("FMS/3,0,1,123"), "capabilities": amf_num(31)}) +
                        amf_object({"level": amf_str("status"), "code": amf_str("NetConnection.Connect.Success"),
                                    "description": amf_str("Connection succeeded."), "objectEncoding": amf_num(0)}))
                send_message(conn, 3, 0, 20, body)
            elif name == "createStream":
                send_message(conn, 3, 0, 20, amf_str("_result") + amf_num(txn) + b"\x05" + amf_num(1))
            elif name == "publish":
                body = (amf_str("onStatus") + amf_num(0) + b"\x05" +
                        amf_object({"level": amf_str("status"), "code": amf_str("NetStream.Publish.Start"),
                                    "description": amf_str("Start publishing")}))
                send_message(conn, 5, stream_id, 20, body)
    except (EOFError, OSError, ValueError) as exc:
        with lock:
            log.write("end %s\n" % exc.__class__.__name__)
    finally:
        try:
            conn.close()
        except OSError:
            pass

sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((sys.argv[1], 0))
sock.listen(8)
with open(sys.argv[2], "w") as f:
    f.write(str(sock.getsockname()[1]))
while True:
    conn, peer = sock.accept()
    with lock:
        log.write("accept %s:%s\n" % peer)
    threading.Thread(target=serve, args=(conn,), daemon=True).start()
PY
	echo "$!" >>"$PIDFILE"
}

# ------------------------------------------------------- connection list
# push_list prints the job's push_connections array as compact JSON. job_field
# prints a Python repr, so an array needs its own read.
push_list() {
	api_get "/api/v1/jobs/$1" |
		python3 -c 'import json,sys; job=json.load(sys.stdin)["job"]; sys.stdout.write(json.dumps(job.get("push_connections") or [], separators=(",",":")))'
}

# push_check <job-id> <label> <mode> [args...] asserts the connection list and
# prints it for the PASS line. Modes:
#   one <target> <result>            exactly one rtmp/session element
#   sequence <target> <result> ...   one element per pair, in this order
#   unreachable <target> ...         every element unreachable, one per target
push_check() {
	local job="$1" label="$2" mode="$3"
	shift 3
	local list
	if ! list="$(push_list "$job")"; then
		fail "$label: could not read push_connections for job $job"
	fi
	if ! LIST="$list" python3 - "$mode" "$@" <<'PY'
import json, os, sys

mode = sys.argv[1]
args = sys.argv[2:]
elements = json.loads(os.environ["LIST"])

def describe(element):
    return "%s/%s/%s/%s" % (element.get("family"), element.get("purpose"),
                            element.get("result"), element.get("target"))

problems = []
if mode == "one":
    if len(elements) != 1:
        problems.append("want exactly 1 element, got %d" % len(elements))
    else:
        element = elements[0]
        if (element.get("family"), element.get("purpose")) != ("rtmp", "session"):
            problems.append("want rtmp/session, got %s" % describe(element))
        if element.get("target") != args[0]:
            problems.append("want target %s, got %s" % (args[0], element.get("target")))
        if element.get("result") != args[1]:
            problems.append("want result %s, got %s" % (args[1], element.get("result")))
elif mode == "sequence":
    wanted = [(args[i], args[i + 1]) for i in range(0, len(args), 2)]
    if len(elements) != len(wanted):
        problems.append("want %d elements, got %d" % (len(wanted), len(elements)))
    for index, (target, result) in enumerate(wanted[:len(elements)]):
        element = elements[index]
        if (element.get("family"), element.get("purpose")) != ("rtmp", "session"):
            problems.append("element %d: want rtmp/session, got %s" % (index, describe(element)))
        if element.get("target") != target:
            problems.append("element %d: want target %s, got %s" % (index, target, element.get("target")))
        if element.get("result") != result:
            problems.append("element %d: want result %s, got %s" % (index, result, element.get("result")))
elif mode == "unreachable":
    if not elements:
        problems.append("want at least one element, got none")
    for index, element in enumerate(elements):
        if (element.get("family"), element.get("purpose")) != ("rtmp", "session"):
            problems.append("element %d: want rtmp/session, got %s" % (index, describe(element)))
        if element.get("result") != "unreachable":
            problems.append("element %d: want result unreachable, got %s" % (index, element.get("result")))
        if element.get("target") not in args:
            problems.append("element %d: target %s is not a judged address" % (index, element.get("target")))
    for target in args:
        if not any(element.get("target") == target for element in elements):
            problems.append("no element for judged address %s" % target)
else:
    problems.append("unknown mode %s" % mode)

if problems:
    sys.stderr.write("; ".join(problems) + "\n")
    sys.exit(1)
PY
	then
		fail "$label: push_connections assertion failed ($mode $*); list: $list; see $CLI_LOG"
	fi
	printf '%s' "$list"
}

# ------------------------------------------------------------- execution
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
exit "$rc"
WRAPPER
chmod +x "$FFMPEG_WRAPPER"

start_server
start_worker tc15-worker \
	RFFMPEG_FFMPEG_PATH="$FFMPEG_WRAPPER" \
	TC15_HOSTS="$HOSTS" \
	TC15_REBIND_HOSTS="$REBIND_HOSTS" \
	TC15_REBIND_FLAG="$REBIND_FLAG" \
	TC15_REAL_FFMPEG="$REAL_FFMPEG" \
	GODEBUG=netdns=go
wait_ready

MEDIA="$(make_media net-output.mp4 1)"

# ------------------------------------------------------- 1. 改指向（AC-61）
# The name resolves to an allowed receiver for the judgement; the push must
# still land there after the name is rewritten to a denied receiver.
set_hosts '127.0.0.1 localhost' '10.77.0.2 tc15-push.test'
settle_hosts

PUSH_PORTFILE="$TMP_ROOT/push.port"
PUSH_LOG="$TMP_ROOT/push.accepts"
PUSH_MEDIA="$TMP_ROOT/push.media"
: >"$PUSH_LOG"
start_rtmp_receiver 10.77.0.2 "$PUSH_PORTFILE" "$PUSH_LOG" "$PUSH_MEDIA"
wait_portfile "$PUSH_PORTFILE"
PUSH_PORT="$(cat "$PUSH_PORTFILE")"

DENIED_LOG="$TMP_ROOT/denied.accepts"
: >"$DENIED_LOG"
start_tcp_counter 127.0.0.1 "$PUSH_PORT" "$TMP_ROOT/denied.port" "$DENIED_LOG"
wait_portfile "$TMP_ROOT/denied.port"

PUSH_TARGET="rtmp://tc15-push.test:$PUSH_PORT/live/key"
printf '127.0.0.1 localhost\n127.0.0.1 tc15-push.test\n' >"$REBIND_HOSTS"

# Control run: the name resolves to the allowed receiver for both the
# judgement and the connection.
rc=0
run_cli "$PUSH_TARGET" flv || rc=$?
expect_completed "control push" "$rc"
CONTROL_LIST="$(push_check "$JOB" "control push" one "10.77.0.2:$PUSH_PORT" connected)"
CONTROL_BYTES="$(media_bytes "$PUSH_MEDIA")"
[[ "$CONTROL_BYTES" -gt 0 ]] || fail "control push: the judged receiver got no media"
[[ "$(accept_count "$DENIED_LOG")" == "0" ]] ||
	fail "control push: the denied receiver was contacted"
echo "PASS: [control] push_connections=$CONTROL_LIST media_bytes=$CONTROL_BYTES denied_accepts=$(accept_count "$DENIED_LOG")"

# Rebind run: judgement still sees 10.77.0.2; before the connection is made the
# same name is rewritten to the denied loopback receiver.
MEDIA_BEFORE="$(media_bytes "$PUSH_MEDIA")"
: >"$REBIND_FLAG"
rc=0
run_cli "$PUSH_TARGET" flv || rc=$?
rm -f "$REBIND_FLAG"
expect_completed "rebound push" "$rc"
REBIND_LIST="$(push_check "$JOB" "rebound push" one "10.77.0.2:$PUSH_PORT" connected)"
REBIND_BYTES="$(media_bytes "$PUSH_MEDIA")"
[[ "$REBIND_BYTES" -gt "$MEDIA_BEFORE" ]] ||
	fail "rebound push: the judged receiver got no new media ($MEDIA_BEFORE → $REBIND_BYTES)"
[[ "$(accept_count "$DENIED_LOG")" == "0" ]] ||
	fail "rebound push: the connection followed the post-judgement name to the denied receiver"
grep -qF '10.77.0.2 tc15-push.test' "$HOSTS" ||
	fail "rebound push: the wrapper did not restore the controlled hosts file"
echo "PASS: [rebind] push_connections=$REBIND_LIST media_bytes=$REBIND_BYTES denied_accepts=$(accept_count "$DENIED_LOG")"

# ------------------------------------------------- 2. 多地址落点（AC-61）
# Two allowed addresses, only the second reachable: the list records the first
# as unreachable and the second as connected, and nothing outside the judged set
# is contacted.
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-multi.test' '10.77.0.2 tc15-multi.test'
settle_hosts

MULTI_PORTFILE="$TMP_ROOT/multi.port"
MULTI_LOG="$TMP_ROOT/multi.accepts"
MULTI_MEDIA="$TMP_ROOT/multi.media"
: >"$MULTI_LOG"
start_rtmp_receiver 10.77.0.2 "$MULTI_PORTFILE" "$MULTI_LOG" "$MULTI_MEDIA"
wait_portfile "$MULTI_PORTFILE"
MULTI_PORT="$(cat "$MULTI_PORTFILE")"

OUTSIDE_LOG="$TMP_ROOT/outside.accepts"
: >"$OUTSIDE_LOG"
start_tcp_counter 10.77.0.3 "$MULTI_PORT" "$TMP_ROOT/outside.port" "$OUTSIDE_LOG"
wait_portfile "$TMP_ROOT/outside.port"

rc=0
run_cli "rtmp://tc15-multi.test:$MULTI_PORT/live/key" flv || rc=$?
expect_completed "multi-address push" "$rc"
MULTI_LIST="$(push_check "$JOB" "multi-address push" sequence \
	"10.77.0.1:$MULTI_PORT" unreachable "10.77.0.2:$MULTI_PORT" connected)"
MULTI_BYTES="$(media_bytes "$MULTI_MEDIA")"
[[ "$MULTI_BYTES" -gt 0 ]] || fail "multi-address push: the second judged address got no media"
[[ "$(accept_count "$OUTSIDE_LOG")" == "0" ]] ||
	fail "multi-address push: a receiver outside the judged set was contacted"
echo "PASS: [multi-address] push_connections=$MULTI_LIST media_bytes=$MULTI_BYTES outside_accepts=$(accept_count "$OUTSIDE_LOG")"

# --------------------------------------------- 3. 全部不可达（AC-61）
# Both judged addresses are unreachable: every element is unreachable and the job
# fails FFMPEG_ERROR, with nothing landing outside the judged set.
set_hosts '127.0.0.1 localhost' '10.77.0.1 tc15-dead.test' '10.77.0.3 tc15-dead.test'
settle_hosts

DEAD_LOG="$TMP_ROOT/dead.accepts"
: >"$DEAD_LOG"
start_tcp_counter 10.77.0.2 0 "$TMP_ROOT/dead.port" "$DEAD_LOG"
wait_portfile "$TMP_ROOT/dead.port"
DEAD_PORT="$(cat "$TMP_ROOT/dead.port")"

rc=0
run_cli "rtmp://tc15-dead.test:$DEAD_PORT/live/key" flv || rc=$?
[[ $rc -eq 1 ]] || fail "all-unreachable push: rc=$rc, want 1; see $CLI_LOG"
JOB="$(job_id_from_log "$CLI_LOG")"
[[ -n "$JOB" ]] || fail "all-unreachable push: no job id in the CLI log"
DEAD_LIST="$(push_check "$JOB" "all-unreachable push" unreachable \
	"10.77.0.1:$DEAD_PORT" "10.77.0.3:$DEAD_PORT")"
[[ "$(job_field "$JOB" failure_type)" == "FFMPEG_ERROR" ]] ||
	fail "all-unreachable push: failure_type $(job_field "$JOB" failure_type), want FFMPEG_ERROR"
[[ "$(accept_count "$DEAD_LOG")" == "0" ]] ||
	fail "all-unreachable push: a receiver outside the judged set was contacted"
echo "PASS: [all-unreachable] push_connections=$DEAD_LIST failure_type=$(job_field "$JOB" failure_type) outside_accepts=$(accept_count "$DEAD_LOG")"

echo "PASS: AC-61: the connection lands on the judged address after the name is rewritten to a denied receiver; a two-address judgement records the unreachable first address and the connected second one and completes on the second; an all-unreachable judged set records every element unreachable and fails FFMPEG_ERROR, all with zero connections outside the judged set"
