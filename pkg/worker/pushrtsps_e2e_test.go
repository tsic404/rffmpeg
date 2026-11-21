package worker

// End-to-end coverage for encrypted RTSP transport handling: the worker must
// hand the request's transport choice to ffmpeg unchanged, and the media must
// stay on the judged connection instead of reaching an address outside the
// judged set.

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"io"
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

// TestRTSPSKeepsRequestNameForTLS pins the C-131 half of the transport change:
// the request's host name is what TLS sees, while the socket still lands on the
// judged address. A literal judged target would prove neither.
func TestRTSPSKeepsRequestNameForTLS(t *testing.T) {
	const requestName = "stream.example.test"
	addr := netip.MustParseAddr("127.0.0.1")
	cert, pool := testTLSCertificate(t, requestName)
	receiver := startTLSReceiverFixture(t, addr, cert)
	requestHost := net.JoinHostPort(requestName, strconv.Itoa(receiver.port()))
	out := jobOutput{target: "rtsps://" + requestHost + "/live", addresses: []netip.Addr{addr}}

	ledger := newPushLedger()
	proxy, err := startJudgedHTTPProxy(out.addresses, out.requestHost(), ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	var w Worker
	attempt, err := w.openPushAttempt(out, out.pushTransport(), addr, proxy, ledger, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.close()
	if attempt.target != out.target {
		t.Fatalf("ffmpeg target = %q, want the request's own name %q", attempt.target, out.target)
	}

	conn, reader, status := dialCONNECT(t, proxy.URL(), requestHost)
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	tlsConn := tls.Client(&bufferedConn{Conn: conn, reader: reader},
		&tls.Config{ServerName: requestName, RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake through the judged proxy: %v", err)
	}
	_ = tlsConn.Close()

	names := receiver.serverNames()
	if len(names) != 1 || names[0] != requestName {
		t.Fatalf("receiver SNI = %v, want [%s]: TLS must see the request host, not the judged address", names, requestName)
	}
	attempt.collect()
	elements := ledger.snapshot()
	if len(elements) != 1 {
		t.Fatalf("push_connections = %+v, want one rtsps session element", elements)
	}
	element := elements[0]
	if element.Family != "rtsps" || element.Purpose != "session" || element.Result != pushResultConnected {
		t.Fatalf("element = %+v, want a connected rtsps session", element)
	}
	if want := net.JoinHostPort(addr.String(), strconv.Itoa(receiver.port())); element.Target != want {
		t.Fatalf("element target = %q, want the judged address %q", element.Target, want)
	}
}

// TestRTSPSKeepsRequestedTransportInFFmpegArguments drives the real pipeline for
// an encrypted RTSP target and reads the argument list ffmpeg actually received:
// an explicit UDP transport must arrive unrewritten, and a request that names no
// transport must not gain one.
func TestRTSPSKeepsRequestedTransportInFFmpegArguments(t *testing.T) {
	addr := privateIPv4(t)
	base := []string{"-i", "<INPUT_FILE>", "-c:v", "mpeg4", "-f", "rtsp"}
	cases := []struct {
		name      string
		transport []string
	}{
		{name: "explicit_udp", transport: []string{"-rtsp_transport", "udp"}},
		{name: "explicit_udp_multicast", transport: []string{"-rtsp_transport", "udp_multicast"}},
		{name: "default", transport: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			recordPath := filepath.Join(dir, "args.txt")
			ffmpegPath, err := writeArgRecordingFFmpeg(dir, recordPath)
			if err != nil {
				t.Fatalf("write argument-recording ffmpeg: %v", err)
			}
			stack := newPushE2EStack(t)
			target := "rtsps://" + net.JoinHostPort(addr.String(), "322") + "/live"
			job := testPushJob(t, stack, "push-rtsps-"+tc.name, target, testUploadID("push-rtsps-"+tc.name))
			job.Args = append(append([]string(nil), base...), tc.transport...)

			w := newFailureMatrixWorker(t, ffmpegPath, stack.server.URL, dir)
			runJob(t, w, job)

			raw, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatalf("ffmpeg arguments were never recorded: %v", err)
			}
			args := strings.Fields(string(raw))
			var transports []string
			for i, arg := range args {
				if arg == "-rtsp_transport" && i+1 < len(args) {
					transports = append(transports, args[i+1])
				}
			}
			want := []string(nil)
			if len(tc.transport) > 0 {
				want = []string{tc.transport[1]}
			}
			if strings.Join(transports, ",") != strings.Join(want, ",") {
				t.Fatalf("ffmpeg -rtsp_transport values = %v, want %v: the worker must not add or rewrite one", transports, want)
			}

			info := stack.getJob(t, job.ID)
			if info.Status != protocol.JobStatusCompleted {
				t.Fatalf("status = %s (%s: %s), want completed: the request must not be refused", info.Status, info.FailureType, info.Error)
			}
		})
	}
}

// rtspsFixtureServer answers the RTSP publish dialogue over TLS and records the
// Transport header and SNI ffmpeg actually sent.
type rtspsFixtureServer struct {
	listener net.Listener
	mu       sync.Mutex
	setups   []string
	sni      []string
	media    int
}

func startRTSPSFixture(t *testing.T, addr netip.Addr, cert tls.Certificate) *rtspsFixtureServer {
	t.Helper()
	raw, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen RTSPS control: %v", err)
	}
	fixture := &rtspsFixtureServer{listener: raw}
	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			fixture.mu.Lock()
			fixture.sni = append(fixture.sni, hello.ServerName)
			fixture.mu.Unlock()
			return nil, nil
		},
	}
	go func() {
		for {
			conn, err := raw.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				tlsConn := tls.Server(conn, config)
				_ = tlsConn.SetDeadline(time.Now().Add(30 * time.Second))
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				fixture.serveRTSP(t, tlsConn)
			}(conn)
		}
	}()
	t.Cleanup(func() { raw.Close() })
	return fixture
}

// serveRTSP answers the muxer's requests and counts the interleaved media
// frames that arrive on the same connection.
func (f *rtspsFixtureServer) serveRTSP(t *testing.T, conn *tls.Conn) {
	reader := bufio.NewReader(conn)
	for {
		head, err := reader.Peek(1)
		if err != nil {
			return
		}
		if head[0] == '$' {
			frame := make([]byte, 4)
			if _, err := io.ReadFull(reader, frame); err != nil {
				return
			}
			length := int(binary.BigEndian.Uint16(frame[2:4]))
			if _, err := io.CopyN(io.Discard, reader, int64(length)); err != nil {
				return
			}
			f.mu.Lock()
			f.media++
			f.mu.Unlock()
			continue
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return
		}
		method := fields[0]
		cseq := "1"
		contentLength := 0
		transport := ""
		for {
			header, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			header = strings.TrimRight(header, "\r\n")
			if header == "" {
				break
			}
			name, value, ok := strings.Cut(header, ":")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "cseq":
				cseq = strings.TrimSpace(value)
			case "content-length":
				contentLength, _ = strconv.Atoi(strings.TrimSpace(value))
			case "transport":
				transport = strings.TrimSpace(value)
			}
		}
		if contentLength > 0 {
			if _, err := io.CopyN(io.Discard, reader, int64(contentLength)); err != nil {
				return
			}
		}
		extra := ""
		switch method {
		case "OPTIONS":
			extra = "Public: OPTIONS, ANNOUNCE, SETUP, RECORD, TEARDOWN\r\n"
		case "SETUP":
			f.mu.Lock()
			f.setups = append(f.setups, transport)
			f.mu.Unlock()
			extra = "Transport: " + transport + "\r\nSession: 12345678\r\n"
		case "ANNOUNCE", "RECORD", "TEARDOWN":
			extra = "Session: 12345678\r\n"
		}
		if _, err := io.WriteString(conn, "RTSP/1.0 200 OK\r\nCSeq: "+cseq+"\r\n"+extra+"\r\n"); err != nil {
			return
		}
	}
}

func (f *rtspsFixtureServer) port() int {
	return f.listener.Addr().(*net.TCPAddr).Port
}

func (f *rtspsFixtureServer) observed() (setups, sni []string, media int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.setups...), append([]string(nil), f.sni...), f.media
}

// TestRTSPSKeepsMediaOnJudgedConnection runs a real ffmpeg publish against a TLS
// receiver on the judged address, once asking for UDP and once leaving the
// transport to ffmpeg's default. Encrypted RTSP has no way to rewrite the SETUP
// destination inside TLS, so the only acceptable outcome is that the media
// stays interleaved on the judged control connection; a UDP media flow would
// leave the judged set.
func TestRTSPSKeepsMediaOnJudgedConnection(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	addr := privateIPv4(t)
	cert, _ := testTLSCertificate(t, "judged.rtsps.test")
	cases := []struct {
		name      string
		transport []string
	}{
		{name: "explicit_udp", transport: []string{"-rtsp_transport", "udp"}},
		{name: "default", transport: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := startRTSPSFixture(t, addr, cert)
			judged := net.JoinHostPort(addr.String(), strconv.Itoa(server.port()))
			target := "rtsps://" + judged + "/live"

			stack := newPushE2EStack(t)
			job := testPushJob(t, stack, "push-rtsps-"+tc.name, target, testUploadID("push-rtsps-"+tc.name))
			// -tls_verify 0 only lets the fixture's self-signed certificate
			// through; nothing here asserts on certificate content.
			job.Args = append([]string{"-i", "<INPUT_FILE>", "-c:v", "mpeg4", "-f", "rtsp"},
				append(append([]string(nil), tc.transport...), "-tls_verify", "0")...)

			w := newFailureMatrixWorker(t, ffmpegPath, stack.server.URL, t.TempDir())
			runJob(t, w, job)

			info := stack.getJob(t, job.ID)
			if info.Status != protocol.JobStatusCompleted {
				t.Fatalf("status = %s (%s: %s %s), want completed", info.Status, info.FailureType, info.Error, info.FailureDetails)
			}
			setups, _, media := server.observed()
			if len(setups) != 1 {
				t.Fatalf("SETUP requests = %v, want one", setups)
			}
			if !strings.HasPrefix(strings.ToUpper(setups[0]), "RTP/AVP/TCP") {
				t.Fatalf("SETUP Transport = %q, want RTP/AVP/TCP: media must stay on the judged control connection", setups[0])
			}
			if media == 0 {
				t.Fatal("receiver saw no interleaved media frames on the judged connection")
			}

			var session *protocol.PushConnection
			for i := range info.PushConnections {
				element := info.PushConnections[i]
				if !pushKindAllowed(element.Family, element.Purpose) {
					t.Fatalf("family/purpose %s/%s is not a C-141 combination: %+v", element.Family, element.Purpose, element)
				}
				if element.Family == "rtsps" && element.Purpose == "session" {
					session = &info.PushConnections[i]
				}
			}
			if session == nil {
				t.Fatalf("push_connections = %+v, want an rtsps session", info.PushConnections)
			}
			if session.Result != pushResultConnected {
				t.Fatalf("rtsps session = %+v, want connected", *session)
			}
			if session.Target != judged {
				t.Fatalf("rtsps session target = %q, want the judged address %q", session.Target, judged)
			}
		})
	}
}
