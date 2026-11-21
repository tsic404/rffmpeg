package worker

// End-to-end coverage for the C-141 rows this worker produces beyond the plain
// session and single-datagram cases: Pro-MPEG FEC, the FTP data connection, and
// RTSP's UDP media pair. Each fixture really speaks enough of the protocol to
// reach a receiver on the judged address, so the list is produced by real
// attempts rather than by asserting on negotiation.

import (
	"bufio"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// writeFixture writes an executable standing in for ffmpeg. These fixtures
// dial with bash's /dev/tcp and /dev/udp redirections, so they declare bash
// rather than the POSIX shell helper: under a shell without those redirections
// the redirection fails and the script never reports the connection attempt.
func writeFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!"+bashFixture+"\n"+body), 0o755); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	if out, err := exec.Command(bashFixture, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("fixture %s rejected by %s -n: %v: %s", name, bashFixture, err, out)
	}
	return path
}

// fixtureTarget renders the shell prologue that splits the target URL into the
// host and port the fixture has to reach.
const fixtureTarget = `target="${!#}"
rest="${target#*://}"
hostport="${rest%%/*}"
hostport="${hostport#*@}"
host="${hostport%%:*}"
host="${host#[}"
host="${host%]}"
port="${hostport##*:}"
`

// listenBlock returns a judged datagram or stream receiver bound to the judged
// address on the first free contiguous block of want ports, so the FEC fixture
// can reach ports base, base+2 and base+4.
func listenUDPBlock(t *testing.T, addr netip.Addr, offsets []int) (int, []*net.UDPConn) {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(addr.AsSlice())})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		base := probe.LocalAddr().(*net.UDPAddr).Port
		_ = probe.Close()
		if base+offsets[len(offsets)-1] > 65535 {
			continue
		}
		conns := make([]*net.UDPConn, 0, len(offsets))
		ok := true
		for _, offset := range offsets {
			conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(addr.AsSlice()), Port: base + offset})
			if err != nil {
				ok = false
				break
			}
			conns = append(conns, conn)
		}
		if ok {
			return base, conns
		}
		for _, conn := range conns {
			conn.Close()
		}
	}
	t.Skip("no free contiguous datagram port block for the FEC fixture")
	return 0, nil
}

// listenLoopbackUDP binds datagram receivers on the loopback at the given
// ports. The loopback is outside the judged set, so a flow that landed there
// instead of on the judged address shows up as a datagram at the trap.
func listenLoopbackUDP(t *testing.T, ports ...int) []*net.UDPConn {
	t.Helper()
	conns := make([]*net.UDPConn, 0, len(ports))
	for _, port := range ports {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			for _, opened := range conns {
				opened.Close()
			}
			t.Fatalf("trap datagram receiver on loopback port %d: %v", port, err)
		}
		conns = append(conns, conn)
	}
	return conns
}

// datagramsOutsideJudgedSet counts the datagrams that reach the traps within
// the window. Every trap is read at the same time, so the window is paid once.
func datagramsOutsideJudgedSet(traps []*net.UDPConn, window time.Duration) int {
	var mu sync.Mutex
	total := 0
	var wg sync.WaitGroup
	for _, conn := range traps {
		wg.Add(1)
		go func(conn *net.UDPConn) {
			defer wg.Done()
			_ = conn.SetReadDeadline(time.Now().Add(window))
			buf := make([]byte, 2048)
			for {
				if _, _, err := conn.ReadFromUDP(buf); err != nil {
					return
				}
				mu.Lock()
				total++
				mu.Unlock()
			}
		}(conn)
	}
	wg.Wait()
	return total
}

// acceptsOutsideJudgedSet counts the connections a loopback trap accepts
// within the window.
func acceptsOutsideJudgedSet(trap net.Listener, window time.Duration) int {
	if tcp, ok := trap.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(window))
	}
	accepted := 0
	for {
		conn, err := trap.Accept()
		if err != nil {
			return accepted
		}
		accepted++
		conn.Close()
	}
}

func TestPushConnectionsRecordProMPEGFECStreams(t *testing.T) {
	addr := privateIPv4(t)
	base, conns := listenUDPBlock(t, addr, []int{0, 1, 2, 4})
	defer func() {
		for _, conn := range conns {
			conn.Close()
		}
	}()
	// The loopback is outside the judged set. A trap on the same port numbers
	// catches a flow that landed on the local relay instead of the judged
	// address (C-133, C-137).
	traps := listenLoopbackUDP(t, base, base+1, base+2, base+4)
	defer func() {
		for _, conn := range traps {
			conn.Close()
		}
	}()

	var wg sync.WaitGroup
	got := make([]chan int, len(conns))
	for i, conn := range conns {
		got[i] = make(chan int, 1)
		wg.Add(1)
		go func(conn *net.UDPConn, out chan<- int) {
			defer wg.Done()
			buf := make([]byte, 64)
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				out <- 0
				return
			}
			out <- n
		}(conn, got[i])
	}

	// The fixture plays the rtp_mpegts muxer with Pro-MPEG FEC: the media goes
	// to the RTP port, RTCP to the next port, and ffmpeg derives the column and
	// row streams from the same RTP port (+2 and +4).
	script := fixtureTarget + `media_port="${port}"
col_port="$((media_port + 2))"
row_port="$((media_port + 4))"
rtcp_port="$((media_port + 1))"
exec 3<>"/dev/udp/${host}/${media_port}" || exit 1
printf 'rtp-media-0123456789' >&3
exec 4<>"/dev/udp/${host}/${col_port}" || exit 1
printf 'fec-column-0123456789' >&4
exec 5<>"/dev/udp/${host}/${row_port}" || exit 1
printf 'fec-row-0123456789' >&5
exec 6<>"/dev/udp/${host}/${rtcp_port}" || exit 1
printf 'rtcp-report-0123456789' >&6
sleep 0.3
exec 3>&-
exec 4>&-
exec 5>&-
exec 6>&-
exit 0
`
	stack := newPushE2EStack(t)
	target := fmt.Sprintf("rtp://%s/live", net.JoinHostPort(addr.String(), strconv.Itoa(base)))
	w := newFailureMatrixWorker(t, writeFixture(t, t.TempDir(), "ffmpeg-fec", script), stack.server.URL, t.TempDir())
	job := testPushJob(t, stack, "push-fec", target, testUploadID("push-fec"))
	job.Args = []string{"-i", "<INPUT_FILE>", "-c:v", "mpeg2video", "-fec", "prompeg=l=4:d=4", "-f", "rtp_mpegts"}

	runJob(t, w, job)
	wg.Wait()

	for i, ch := range got {
		if n := <-ch; n == 0 {
			t.Fatalf("judged receiver %d got no datagram", i)
		}
	}
	if outside := datagramsOutsideJudgedSet(traps, 300*time.Millisecond); outside != 0 {
		t.Fatalf("%d datagrams reached the loopback traps, want 0: every flow belongs on the judged address", outside)
	}
	info := stack.getJob(t, job.ID)
	if info.Status != protocol.JobStatusCompleted {
		t.Fatalf("status = %s, want completed", info.Status)
	}
	byKind := map[string]protocol.PushConnection{}
	var fecTargets []string
	for _, element := range info.PushConnections {
		if !pushKindAllowed(element.Family, element.Purpose) {
			t.Fatalf("family/purpose %s/%s is not a C-141 combination: %+v", element.Family, element.Purpose, element)
		}
		if element.Origin != "request" {
			t.Fatalf("element origin = %q, want request (FEC ports are derived from the request): %+v", element.Origin, element)
		}
		byKind[element.Family+"/"+element.Purpose] = element
		if element.Family == "fec" {
			fecTargets = append(fecTargets, element.Target)
		}
	}
	stream, ok := byKind["rtp/stream"]
	if !ok {
		t.Fatalf("push_connections = %+v, want an rtp stream element", info.PushConnections)
	}
	if want := net.JoinHostPort(addr.String(), strconv.Itoa(base)); stream.Target != want {
		t.Fatalf("rtp target = %q, want the judged address %q", stream.Target, want)
	}
	rtcp, ok := byKind["rtcp/derived"]
	if !ok {
		t.Fatalf("push_connections = %+v, want the standalone RTP's rtcp derived element", info.PushConnections)
	}
	if want := net.JoinHostPort(addr.String(), strconv.Itoa(base+1)); rtcp.Target != want {
		t.Fatalf("rtcp target = %q, want the judged address %q", rtcp.Target, want)
	}
	if rtcp.Result != "sent" {
		t.Fatalf("rtcp result = %q, want sent", rtcp.Result)
	}
	wantFEC := map[string]bool{
		net.JoinHostPort(addr.String(), strconv.Itoa(base+2)): true,
		net.JoinHostPort(addr.String(), strconv.Itoa(base+4)): true,
	}
	if len(fecTargets) != len(wantFEC) {
		t.Fatalf("fec targets = %v, want the column and row ports on the judged address", fecTargets)
	}
	for _, target := range fecTargets {
		if !wantFEC[target] {
			t.Fatalf("fec target = %q, want one of the judged FEC ports", target)
		}
		delete(wantFEC, target)
	}
	for _, element := range info.PushConnections {
		if element.Family == "fec" && element.Result != "sent" {
			t.Fatalf("fec element = %+v, want sent", element)
		}
	}
}

// ftpFixtureServer answers the control dialogue ffmpeg needs to open a passive
// data connection and counts the payload the data connection delivers.
type ftpFixtureServer struct {
	listener  *net.TCPListener
	dataPort  int
	dataBytes chan int
	control   chan string
	// pasvHost is the address the server announces in a 227 reply. It defaults
	// to the judged address; a test points it elsewhere to play a server that
	// guides the data connection off the judged set (C-130).
	pasvMu   sync.Mutex
	pasvHost netip.Addr
	pasvPort int
}

// setPASVGuidance makes the server announce addr and port in its 227 replies.
// A zero port keeps the fixture's own data port.
func (s *ftpFixtureServer) setPASVGuidance(addr netip.Addr, port int) {
	s.pasvMu.Lock()
	s.pasvHost = addr
	s.pasvPort = port
	s.pasvMu.Unlock()
}

func (s *ftpFixtureServer) pasvAddress() (netip.Addr, int) {
	s.pasvMu.Lock()
	defer s.pasvMu.Unlock()
	port := s.pasvPort
	if port == 0 {
		port = s.dataPort
	}
	return s.pasvHost, port
}

func startFTPFixture(t *testing.T, addr netip.Addr) *ftpFixtureServer {
	t.Helper()
	control, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen FTP control: %v", err)
	}
	data, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		control.Close()
		t.Fatalf("listen FTP data: %v", err)
	}
	server := &ftpFixtureServer{
		listener:  control.(*net.TCPListener),
		dataPort:  data.Addr().(*net.TCPAddr).Port,
		dataBytes: make(chan int, 1),
		control:   make(chan string, 16),
		pasvHost:  addr,
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		conn, err := data.Accept()
		if err != nil {
			server.dataBytes <- 0
			return
		}
		defer conn.Close()
		total := 0
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			n, err := conn.Read(buf)
			total += n
			if err != nil {
				break
			}
		}
		server.dataBytes <- total
	}()
	go func() {
		defer wg.Done()
		conn, err := control.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
		write("220 fixture ftp")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			server.control <- line
			verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
			switch verb {
			case "USER":
				write("331 password please")
			case "PASS":
				write("230 logged in")
			case "FEAT":
				write("211-Features:")
				write(" EPSV")
				write("211 End")
			case "TYPE":
				write("200 type set")
			case "PWD":
				write("257 \"/\" is current")
			case "REST":
				write("350 restart ok")
			case "SIZE":
				write("213 0")
			case "EPSV":
				write(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", server.dataPort))
			case "PASV":
				announced, port := server.pasvAddress()
				host := announced.As4()
				write(fmt.Sprintf("227 Entering Passive Mode (%d,%d,%d,%d,%d,%d)",
					host[0], host[1], host[2], host[3],
					port/256, port%256))
			case "STOR", "APPE":
				write("150 opening")
				received := <-server.dataBytes
				server.dataBytes <- received
				write("226 done")
			case "QUIT":
				write("221 bye")
				return
			default:
				write("200 ok")
			}
		}
	}()
	t.Cleanup(func() {
		control.Close()
		data.Close()
		wg.Wait()
	})
	return server
}

func TestPushConnectionsRecordFTPDataConnection(t *testing.T) {
	addr := privateIPv4(t)
	server := startFTPFixture(t, addr)

	script := fixtureTarget + `exec 3<>"/dev/tcp/${host}/${port}" || exit 1
IFS= read -r line <&3
printf 'USER anonymous\r\nPASS guest\r\nTYPE I\r\nSIZE /out.ts\r\nEPSV\r\n' >&3
data_port=""
while IFS= read -r line <&3; do
  case "$line" in
    229*) data_port=$(printf '%s' "$line" | sed -n 's/.*(|||\([0-9]*\)|).*/\1/p'); break;;
  esac
done
[ -n "$data_port" ] || exit 1
exec 4<>"/dev/tcp/127.0.0.1/${data_port}" || exit 1
printf 'rffmpeg-ftp-data-payload' >&4
sleep 0.3
exec 4>&-
printf 'QUIT\r\n' >&3
exec 3>&-
exit 0
`
	stack := newPushE2EStack(t)
	target := fmt.Sprintf("ftp://user:%s@%s/out.ts", "pass", net.JoinHostPort(addr.String(), strconv.Itoa(server.listener.Addr().(*net.TCPAddr).Port)))
	w := newFailureMatrixWorker(t, writeFixture(t, t.TempDir(), "ffmpeg-ftp", script), stack.server.URL, t.TempDir())
	job := testPushJob(t, stack, "push-ftp", target, testUploadID("push-ftp"))
	job.Args = []string{"-i", "<INPUT_FILE>", "-c:v", "mpeg4", "-f", "mpegts"}

	runJob(t, w, job)

	if n := <-server.dataBytes; n == 0 {
		t.Fatal("the judged data receiver got no payload")
	}
	info := stack.getJob(t, job.ID)
	byKind := map[string]protocol.PushConnection{}
	for _, element := range info.PushConnections {
		if !pushKindAllowed(element.Family, element.Purpose) {
			t.Fatalf("family/purpose %s/%s is not a C-141 combination: %+v", element.Family, element.Purpose, element)
		}
		byKind[element.Family+"/"+element.Purpose] = element
	}
	session, ok := byKind["ftp/session"]
	if !ok {
		t.Fatalf("push_connections = %+v, want an ftp session element", info.PushConnections)
	}
	if session.Result != "connected" {
		t.Fatalf("control connection = %+v, want connected", session)
	}
	data, ok := byKind["ftp/derived"]
	if !ok {
		t.Fatalf("push_connections = %+v, want an ftp derived element", info.PushConnections)
	}
	if data.Result != "connected" {
		t.Fatalf("data connection = %+v, want connected", data)
	}
	if data.Origin != "server" {
		t.Fatalf("data connection origin = %q, want server (EPSV guidance)", data.Origin)
	}
	if want := net.JoinHostPort(addr.String(), strconv.Itoa(server.dataPort)); data.Target != want {
		t.Fatalf("data target = %q, want the judged address %q", data.Target, want)
	}
}

// TestPushFTPPassiveGuidanceOutsideJudgedSetIsBlocked covers C-130 for the FTP
// data connection. The server's 227 reply names a live loopback receiver —
// reachable, but outside the judged set — so the worker answers the PASV with a
// refusal, never dials the announced address (the trap stays empty), records
// the blocked element with origin=server, and the job fails FFMPEG_ERROR.
func TestPushFTPPassiveGuidanceOutsideJudgedSetIsBlocked(t *testing.T) {
	addr := privateIPv4(t)
	trap, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen data trap: %v", err)
	}
	defer trap.Close()
	trapPort := trap.Addr().(*net.TCPAddr).Port
	server := startFTPFixture(t, addr)
	server.setPASVGuidance(netip.MustParseAddr("127.0.0.1"), trapPort)

	// The fixture plays ffmpeg: it asks for a passive connection and dials
	// whatever the reply names, so an honoured outside guidance shows up on the
	// trap instead of only in a list assertion.
	script := fixtureTarget + `exec 3<>"/dev/tcp/${host}/${port}" || exit 1
IFS= read -r line <&3
printf 'USER anonymous\r\n' >&3; IFS= read -r line <&3
printf 'PASS guest\r\n' >&3; IFS= read -r line <&3
printf 'TYPE I\r\n' >&3; IFS= read -r line <&3
printf 'PASV\r\n' >&3; IFS= read -r line <&3
data_host=""
data_port=""
case "$line" in
  227*)
    numbers="${line#*(}"
    numbers="${numbers%%)*}"
    data_host=$(printf '%s' "$numbers" | cut -d, -f1-4 | tr ',' '.')
    high=$(printf '%s' "$numbers" | cut -d, -f5)
    low=$(printf '%s' "$numbers" | cut -d, -f6)
    data_port=$((high * 256 + low))
    ;;
esac
if [ -n "$data_port" ]; then
  exec 4<>"/dev/tcp/${data_host}/${data_port}" || exit 1
  printf 'rffmpeg-ftp-data-payload' >&4
  exec 4>&-
fi
printf 'QUIT\r\n' >&3
exec 3>&-
exit 1
`

	stack := newPushE2EStack(t)
	target := fmt.Sprintf("ftp://user:%s@%s/out.ts", "pass",
		net.JoinHostPort(addr.String(), strconv.Itoa(server.listener.Addr().(*net.TCPAddr).Port)))
	w := newFailureMatrixWorker(t, writeFixture(t, t.TempDir(), "ffmpeg-ftp-pasv", script), stack.server.URL, t.TempDir())
	job := testPushJob(t, stack, "push-ftp-pasv", target, testUploadID("push-ftp-pasv"))
	job.Args = []string{"-i", "<INPUT_FILE>", "-c:v", "mpeg4", "-f", "mpegts"}

	runJob(t, w, job)

	if accepted := acceptsOutsideJudgedSet(trap, 300*time.Millisecond); accepted != 0 {
		t.Fatalf("the guided loopback data destination accepted %d connections, want 0", accepted)
	}
	info := stack.getJob(t, job.ID)
	if info.Status != protocol.JobStatusFailed {
		t.Fatalf("status = %s, want failed", info.Status)
	}
	if info.FailureType != string(protocol.FailureFFmpegError) {
		t.Fatalf("failure_type = %q (error=%q), want FFMPEG_ERROR; list=%+v", info.FailureType, info.Error, info.PushConnections)
	}
	var blocked []protocol.PushConnection
	for _, element := range info.PushConnections {
		if !pushKindAllowed(element.Family, element.Purpose) {
			t.Fatalf("family/purpose %s/%s is not a C-141 combination: %+v", element.Family, element.Purpose, element)
		}
		if element.Result == "blocked" {
			blocked = append(blocked, element)
		}
	}
	if len(blocked) != 1 {
		t.Fatalf("push_connections = %+v, want one blocked ftp data element", info.PushConnections)
	}
	if blocked[0].Family != "ftp" || blocked[0].Purpose != "derived" {
		t.Fatalf("blocked element = %+v, want ftp/derived", blocked[0])
	}
	if blocked[0].Origin != "server" {
		t.Fatalf("blocked element origin = %q, want server (PASV guidance)", blocked[0].Origin)
	}
	if want := net.JoinHostPort("127.0.0.1", strconv.Itoa(trapPort)); blocked[0].Target != want {
		t.Fatalf("blocked element target = %q, want the guided address %q", blocked[0].Target, want)
	}
}

// rtspFixtureServer answers the RTSP dialogue ffmpeg needs, and receives the
// RTP/RTCP datagrams the worker relays to the judged address.
type rtspFixtureServer struct {
	listener  *net.TCPListener
	rtp       *net.UDPConn
	rtcp      *net.UDPConn
	rtpBytes  chan int
	rtcpBytes chan int
}

func startRTSPFixture(t *testing.T, addr netip.Addr) *rtspFixtureServer {
	t.Helper()
	control, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen RTSP control: %v", err)
	}
	rtp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(addr.AsSlice())})
	if err != nil {
		control.Close()
		t.Fatalf("listen RTP: %v", err)
	}
	rtcp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(addr.AsSlice())})
	if err != nil {
		control.Close()
		rtp.Close()
		t.Fatalf("listen RTCP: %v", err)
	}
	server := &rtspFixtureServer{
		listener:  control.(*net.TCPListener),
		rtp:       rtp,
		rtcp:      rtcp,
		rtpBytes:  make(chan int, 1),
		rtcpBytes: make(chan int, 1),
	}
	read := func(conn *net.UDPConn, out chan<- int) {
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			out <- 0
			return
		}
		out <- n
	}
	go read(rtp, server.rtpBytes)
	go read(rtcp, server.rtcpBytes)

	rtpPort := rtp.LocalAddr().(*net.UDPAddr).Port
	rtcpPort := rtcp.LocalAddr().(*net.UDPAddr).Port
	go func() {
		conn, err := control.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			head, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			method := strings.Fields(head)[0]
			cseq := "1"
			contentLength := 0
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSpace(line)
				if line == "" {
					break
				}
				name, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				if strings.EqualFold(name, "CSeq") {
					cseq = strings.TrimSpace(value)
				}
				if strings.EqualFold(name, "Content-Length") {
					contentLength, _ = strconv.Atoi(strings.TrimSpace(value))
				}
			}
			if contentLength > 0 {
				body := make([]byte, contentLength)
				if _, err := reader.Read(body); err != nil {
					return
				}
			}
			extra := ""
			switch method {
			case "OPTIONS":
				extra = "Public: OPTIONS, ANNOUNCE, SETUP, RECORD, TEARDOWN\r\n"
			case "SETUP":
				extra = fmt.Sprintf("Transport: RTP/AVP/UDP;unicast;mode=record;server_port=%d-%d;source=%s\r\nSession: 12345678\r\n",
					rtpPort, rtcpPort, addr.String())
			case "ANNOUNCE", "RECORD", "TEARDOWN":
				extra = "Session: 12345678\r\n"
			}
			_, _ = conn.Write([]byte(fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %s\r\n%s\r\n", cseq, extra)))
		}
	}()
	t.Cleanup(func() {
		control.Close()
		rtp.Close()
		rtcp.Close()
	})
	return server
}

func TestPushConnectionsRecordRTSPUDPMediaAsDerived(t *testing.T) {
	addr := privateIPv4(t)
	server := startRTSPFixture(t, addr)
	// A trap on the loopback at the judged media ports must stay empty: media
	// belongs to the judged address, not to a local socket at the same port.
	traps := listenLoopbackUDP(t,
		server.rtp.LocalAddr().(*net.UDPAddr).Port,
		server.rtcp.LocalAddr().(*net.UDPAddr).Port)
	defer func() {
		for _, conn := range traps {
			conn.Close()
		}
	}()

	// The fixture drives the RTSP handshake and then sends media to the
	// destination named in the SETUP reply, which is the worker's relay.
	script := fixtureTarget + `exec 3<>"/dev/tcp/${host}/${port}" || exit 1
read_head() { while IFS= read -r line <&3; do line="${line%$'\r'}"; [ -z "$line" ] && break; done; }
printf 'OPTIONS rtsp://%s/live RTSP/1.0\r\nCSeq: 1\r\n\r\n' "$host" >&3
read_head
printf 'ANNOUNCE rtsp://%s/live RTSP/1.0\r\nCSeq: 2\r\nContent-Length: 0\r\n\r\n' "$host" >&3
read_head
printf 'SETUP rtsp://%s/live RTSP/1.0\r\nCSeq: 3\r\nTransport: RTP/AVP/UDP;unicast;client_port=5000-5001;mode=record\r\n\r\n' "$host" >&3
rtp_port=""
rtcp_port=""
source=""
while IFS= read -r line <&3; do
  line="${line%$'\r'}"
  [ -z "$line" ] && break
  case "$line" in
    Transport:*) rtp_port=$(printf '%s' "$line" | sed -n 's/.*server_port=\([0-9]*\)-\([0-9]*\).*/\1/p')
                 rtcp_port=$(printf '%s' "$line" | sed -n 's/.*server_port=\([0-9]*\)-\([0-9]*\).*/\2/p')
                 source=$(printf '%s' "$line" | sed -n 's/.*source=\([^;]*\).*/\1/p');;
  esac
done
[ -n "$rtp_port" ] || exit 1
exec 4<>"/dev/udp/${source}/${rtp_port}" || exit 1
printf 'rtsp-rtp-payload' >&4
exec 5<>"/dev/udp/${source}/${rtcp_port}" || exit 1
printf 'rtsp-rtcp-payload' >&5
printf 'RECORD rtsp://%s/live RTSP/1.0\r\nCSeq: 4\r\n\r\n' "$host" >&3
read_head
sleep 0.3
printf 'TEARDOWN rtsp://%s/live RTSP/1.0\r\nCSeq: 5\r\n\r\n' "$host" >&3
exec 4>&-
exec 5>&-
exec 3>&-
exit 0
`
	stack := newPushE2EStack(t)
	target := fmt.Sprintf("rtsp://%s/live", net.JoinHostPort(addr.String(), strconv.Itoa(server.listener.Addr().(*net.TCPAddr).Port)))
	w := newFailureMatrixWorker(t, writeFixture(t, t.TempDir(), "ffmpeg-rtsp", script), stack.server.URL, t.TempDir())
	job := testPushJob(t, stack, "push-rtsp-udp", target, testUploadID("push-rtsp-udp"))
	job.Args = []string{"-i", "<INPUT_FILE>", "-c:v", "mpeg4", "-f", "rtsp", "-rtsp_transport", "udp"}

	runJob(t, w, job)

	if n := <-server.rtpBytes; n == 0 {
		t.Fatal("the judged RTP receiver got no datagram")
	}
	if n := <-server.rtcpBytes; n == 0 {
		t.Fatal("the judged RTCP receiver got no datagram")
	}
	if outside := datagramsOutsideJudgedSet(traps, 300*time.Millisecond); outside != 0 {
		t.Fatalf("%d datagrams reached the loopback traps, want 0", outside)
	}
	info := stack.getJob(t, job.ID)
	byKind := map[string]protocol.PushConnection{}
	for _, element := range info.PushConnections {
		if !pushKindAllowed(element.Family, element.Purpose) {
			t.Fatalf("family/purpose %s/%s is not a C-141 combination: %+v", element.Family, element.Purpose, element)
		}
		byKind[element.Family+"/"+element.Purpose] = element
	}
	session, ok := byKind["rtsp/session"]
	if !ok || session.Result != "connected" {
		t.Fatalf("push_connections = %+v, want a connected rtsp session", info.PushConnections)
	}
	for _, family := range []string{"rtp", "rtcp"} {
		element, ok := byKind[family+"/derived"]
		if !ok {
			t.Fatalf("push_connections = %+v, want a %s derived element", info.PushConnections, family)
		}
		if element.Result != "sent" {
			t.Fatalf("%s element = %+v, want sent", family, element)
		}
		// The ports come from the SETUP reply's server_port, not from the
		// request, so the element records the server as its origin (C-130).
		if element.Origin != "server" {
			t.Fatalf("%s element origin = %q, want server (SETUP server_port)", family, element.Origin)
		}
		if strings.Contains(element.Target, "127.0.0.1") {
			t.Fatalf("%s target = %q, which is the loopback relay, not the judged address", family, element.Target)
		}
	}
}

// TestPushGuidedSourceOutsideJudgedSetIsBlocked covers C-130 with a real
// server-guided target: the SETUP reply names a source outside the judged set,
// so no media is sent there, the element is blocked with origin=server, and the
// job fails FFMPEG_ERROR with a non-empty list. The guided destination is a
// live receiver, so an honoured guidance would be visible as a datagram rather
// than only as a missing one.
func TestPushGuidedSourceOutsideJudgedSetIsBlocked(t *testing.T) {
	addr := privateIPv4(t)
	control, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen RTSP control: %v", err)
	}
	defer control.Close()
	// The guided destination is reachable but outside the judged set: the
	// loopback trap must stay empty.
	rtpTrap, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen RTP trap: %v", err)
	}
	defer rtpTrap.Close()
	rtcpTrap, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen RTCP trap: %v", err)
	}
	defer rtcpTrap.Close()
	outside := "127.0.0.1"
	rtpPort := rtpTrap.LocalAddr().(*net.UDPAddr).Port
	rtcpPort := rtcpTrap.LocalAddr().(*net.UDPAddr).Port
	go func() {
		conn, err := control.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			head, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			method := strings.Fields(head)[0]
			cseq := "1"
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSpace(line)
				if line == "" {
					break
				}
				if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "CSeq") {
					cseq = strings.TrimSpace(value)
				}
			}
			extra := ""
			if method == "SETUP" {
				extra = fmt.Sprintf("Transport: RTP/AVP/UDP;unicast;mode=record;server_port=%d-%d;source=%s\r\nSession: 12345678\r\n",
					rtpPort, rtcpPort, outside)
			}
			_, _ = conn.Write([]byte(fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %s\r\n%s\r\n", cseq, extra)))
		}
	}()

	target := fmt.Sprintf("rtsp://%s/live", net.JoinHostPort(addr.String(), strconv.Itoa(control.Addr().(*net.TCPAddr).Port)))
	stack := newPushE2EStack(t)
	job := testPushJob(t, stack, "push-guided", target, testUploadID("push-guided"))
	job.Args = []string{"-i", "<INPUT_FILE>", "-c:v", "mpeg4", "-f", "rtsp", "-rtsp_transport", "udp"}

	w := newFailureMatrixWorker(t, writeFixture(t, t.TempDir(), "ffmpeg-guided", fixtureTarget+`exec 3<>"/dev/tcp/${host}/${port}" || exit 1
read_head() { while IFS= read -r line <&3; do line="${line%$'\r'}"; [ -z "$line" ] && break; done; }
printf 'OPTIONS rtsp://%s/live RTSP/1.0\r\nCSeq: 1\r\n\r\n' "$host" >&3
read_head
printf 'ANNOUNCE rtsp://%s/live RTSP/1.0\r\nCSeq: 2\r\nContent-Length: 0\r\n\r\n' "$host" >&3
read_head
printf 'SETUP rtsp://%s/live RTSP/1.0\r\nCSeq: 3\r\nTransport: RTP/AVP/UDP;unicast;client_port=5000-5001;mode=record\r\n\r\n' "$host" >&3
while IFS= read -r line <&3; do line="${line%$'\r'}"; [ -z "$line" ] && break; done
exec 3>&-
exit 1
`), stack.server.URL, t.TempDir())
	runJob(t, w, job)

	if datagrams := datagramsOutsideJudgedSet([]*net.UDPConn{rtpTrap, rtcpTrap}, 300*time.Millisecond); datagrams != 0 {
		t.Fatalf("%d datagrams reached the guided loopback destination, want 0", datagrams)
	}
	info := stack.getJob(t, job.ID)
	if info.Status != protocol.JobStatusFailed {
		t.Fatalf("status = %s, want failed", info.Status)
	}
	if info.FailureType != string(protocol.FailureFFmpegError) {
		t.Fatalf("failure_type = %q, want FFMPEG_ERROR", info.FailureType)
	}
	if len(info.PushConnections) == 0 {
		t.Fatal("push_connections is empty; a server-guided blocked destination must be recorded")
	}
	var blocked []protocol.PushConnection
	for _, element := range info.PushConnections {
		if !pushKindAllowed(element.Family, element.Purpose) {
			t.Fatalf("family/purpose %s/%s is not a C-141 combination: %+v", element.Family, element.Purpose, element)
		}
		if element.Result == "blocked" {
			blocked = append(blocked, element)
		}
	}
	if len(blocked) != 2 {
		t.Fatalf("push_connections = %+v, want one blocked rtp and one blocked rtcp", info.PushConnections)
	}
	for _, element := range blocked {
		if element.Origin != "server" {
			t.Fatalf("blocked element origin = %q, want server", element.Origin)
		}
		if element.Target != net.JoinHostPort(outside, strconv.Itoa(rtpPort)) &&
			element.Target != net.JoinHostPort(outside, strconv.Itoa(rtcpPort)) {
			t.Fatalf("blocked element target = %q, want the guided address", element.Target)
		}
	}
}

// TestPushRequestProxyOutsideJudgedSetIsBlocked covers C-140 with the request's
// own `-http_proxy`: the proxy address is outside the judged set, so no
// connection is attempted, the element is blocked, and the job fails
// FFMPEG_ERROR without touching either destination.
func TestPushRequestProxyOutsideJudgedSetIsBlocked(t *testing.T) {
	addr := privateIPv4(t)
	receiver, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen receiver: %v", err)
	}
	defer receiver.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := receiver.Accept()
		if err != nil {
			return
		}
		conn.Close()
		accepted <- struct{}{}
	}()

	outsideProxy, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen outside proxy: %v", err)
	}
	defer outsideProxy.Close()
	proxyHits := make(chan struct{}, 1)
	go func() {
		conn, err := outsideProxy.Accept()
		if err != nil {
			return
		}
		conn.Close()
		proxyHits <- struct{}{}
	}()

	target := fmt.Sprintf("http://%s/live", net.JoinHostPort(addr.String(), strconv.Itoa(receiver.Addr().(*net.TCPAddr).Port)))
	stack := newPushE2EStack(t)
	job := testPushJob(t, stack, "push-request-proxy", target, testUploadID("push-request-proxy"))
	job.Args = []string{"-i", "<INPUT_FILE>", "-c:v", "mpeg4", "-f", "mpegts",
		"-http_proxy", "http://192.0.2.88:3128"}

	w := newFailureMatrixWorker(t, writeFixture(t, t.TempDir(), "ffmpeg-proxy", fixtureTarget+`exec 3<>"/dev/tcp/${host}/${port}" || exit 1
printf 'payload' >&3
exec 3>&-
exit 0
`), stack.server.URL, t.TempDir())
	runJob(t, w, job)

	select {
	case <-accepted:
		t.Fatal("the judged receiver was connected even though the request proxy is outside the judged set")
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case <-proxyHits:
		t.Fatal("the outside proxy was connected")
	case <-time.After(300 * time.Millisecond):
	}
	info := stack.getJob(t, job.ID)
	if info.Status != protocol.JobStatusFailed {
		t.Fatalf("status = %s, want failed", info.Status)
	}
	if info.FailureType != string(protocol.FailureFFmpegError) {
		t.Fatalf("failure_type = %q (error=%q), want FFMPEG_ERROR; list=%+v", info.FailureType, info.Error, info.PushConnections)
	}
	if len(info.PushConnections) != 1 {
		t.Fatalf("push_connections = %+v, want one blocked element", info.PushConnections)
	}
	element := info.PushConnections[0]
	if element.Result != "blocked" || element.Origin != "request" {
		t.Fatalf("element = %+v, want a request-origin blocked element", element)
	}
	if element.Target != "192.0.2.88:3128" {
		t.Fatalf("element target = %q, want the proxy address", element.Target)
	}
}

// TestPushIgnoresInheritedProxyEnvironment covers C-140 for the deployment
// where the worker itself runs behind a proxy: with http_proxy naming a host
// outside the judged set, an HTTP push must still reach the judged receiver
// directly and never contact that proxy.
func TestPushIgnoresInheritedProxyEnvironment(t *testing.T) {
	addr := privateIPv4(t)
	receiver, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen receiver: %v", err)
	}
	defer receiver.Close()
	requests := make(chan int, 1)
	go func() {
		conn, err := receiver.Accept()
		if err != nil {
			requests <- 0
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		requests <- n
	}()

	outsideProxy, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen outside proxy: %v", err)
	}
	defer outsideProxy.Close()
	proxyHits := make(chan struct{}, 1)
	go func() {
		conn, err := outsideProxy.Accept()
		if err != nil {
			return
		}
		conn.Close()
		proxyHits <- struct{}{}
	}()

	// The worker's own environment carries the proxy; the request names none.
	inherited := fmt.Sprintf("http_proxy=http://%s:%d",
		addr.String(), outsideProxy.Addr().(*net.TCPAddr).Port)
	t.Setenv("http_proxy", inherited)
	t.Setenv("HTTP_PROXY", inherited)

	target := fmt.Sprintf("http://%s/live", net.JoinHostPort(addr.String(), strconv.Itoa(receiver.Addr().(*net.TCPAddr).Port)))
	stack := newPushE2EStack(t)
	job := testPushJob(t, stack, "push-inherited-proxy", target, testUploadID("push-inherited-proxy"))
	job.Args = []string{"-i", "<INPUT_FILE>", "-c:v", "mpeg4", "-f", "mpegts"}

	// The fixture plays ffmpeg's own proxy handling: when http_proxy is set it
	// connects there instead of to the target, which is exactly the connection
	// the worker has to prevent.
	w := newFailureMatrixWorker(t, writeFixture(t, t.TempDir(), "ffmpeg-inherited", fixtureTarget+`dial_host="${host}"
dial_port="${port}"
if [ -n "${http_proxy:-}" ]; then
  proxy="${http_proxy#*://}"
  proxy="${proxy%%/*}"
  proxy="${proxy#*@}"
  dial_host="${proxy%%:*}"
  dial_port="${proxy##*:}"
fi
exec 3<>"/dev/tcp/${dial_host}/${dial_port}" || exit 1
printf 'GET /live HTTP/1.1\r\nHost: judged\r\n\r\n' >&3
exec 3>&-
exit 0
`), stack.server.URL, t.TempDir())
	runJob(t, w, job)

	select {
	case <-proxyHits:
		t.Fatal("the inherited proxy was contacted; a push may only use a proxy the request named")
	case <-time.After(500 * time.Millisecond):
	}
	if n := <-requests; n == 0 {
		t.Fatal("the judged receiver got no request")
	}
	info := stack.getJob(t, job.ID)
	if info.Status != protocol.JobStatusCompleted {
		t.Fatalf("status = %s, want completed", info.Status)
	}
	for _, element := range info.PushConnections {
		if element.Target != net.JoinHostPort(addr.String(), strconv.Itoa(receiver.Addr().(*net.TCPAddr).Port)) {
			t.Fatalf("element = %+v, want the judged receiver as the only connection", element)
		}
	}
}
