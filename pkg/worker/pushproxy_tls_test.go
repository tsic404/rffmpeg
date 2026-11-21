package worker

// Coverage for the two TLS findings on the proxy paths: the name the request
// used must reach the TLS layer (C-131), and a tunnel only counts as connected
// once the channel and its handshake completed (C-140/C-142).

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testTLSCertificate(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return cert, pool
}

// tlsReceiverFixture records the SNI of every completed handshake.
type tlsReceiverFixture struct {
	listener net.Listener
	mu       sync.Mutex
	names    []string
	accepted int
}

func startTLSReceiverFixture(t *testing.T, addr netip.Addr, cert tls.Certificate) *tlsReceiverFixture {
	t.Helper()
	raw, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen TLS receiver: %v", err)
	}
	fixture := &tlsReceiverFixture{listener: raw}
	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			fixture.mu.Lock()
			fixture.names = append(fixture.names, hello.ServerName)
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
			fixture.mu.Lock()
			fixture.accepted++
			fixture.mu.Unlock()
			go func(conn net.Conn) {
				defer conn.Close()
				tlsConn := tls.Server(conn, config)
				_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				buf := make([]byte, 64)
				_, _ = tlsConn.Read(buf)
			}(conn)
		}
	}()
	t.Cleanup(func() { raw.Close() })
	return fixture
}

func (f *tlsReceiverFixture) port() int {
	return f.listener.Addr().(*net.TCPAddr).Port
}

func (f *tlsReceiverFixture) serverNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

// dialCONNECT opens a tunnel through proxy and consumes the proxy's response
// head, leaving conn positioned at the first tunnelled byte.
func dialCONNECT(t *testing.T, proxyURL, target string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		conn.Close()
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		t.Fatalf("read CONNECT response: %v", err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return conn, reader, strings.TrimSpace(status)
}

// TestJudgedProxyTLSKeepsRequestHostname is the C-131 assertion for the path
// TLS families now take: the socket lands on the judged address while SNI and
// certificate verification see the host the request named.
func TestJudgedProxyTLSKeepsRequestHostname(t *testing.T) {
	const requestName = "stream.example.test"
	addr := netip.MustParseAddr("127.0.0.1")
	cert, pool := testTLSCertificate(t, requestName)
	receiver := startTLSReceiverFixture(t, addr, cert)
	requestHost := net.JoinHostPort(requestName, strconv.Itoa(receiver.port()))
	out := jobOutput{target: "rtmps://" + requestHost + "/live"}

	ledger := newPushLedger()
	proxy, err := startJudgedHTTPProxy([]netip.Addr{addr}, requestHost, ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	// Open the attempt the way the worker does for a TLS family. The target
	// ffmpeg receives must still carry the request's host name; replacing it
	// with the relay address is exactly the defect this records (C-131).
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
	if _, err := tlsConn.Write([]byte("rtmps-payload")); err != nil {
		t.Fatalf("write after handshake: %v", err)
	}
	_ = tlsConn.Close()

	names := receiver.serverNames()
	if len(names) != 1 || names[0] != requestName {
		t.Fatalf("receiver SNI = %v, want [%s]: TLS must see the request host, not the judged address", names, requestName)
	}
	if attempted, established := proxy.Observed(); !attempted || !established {
		t.Fatalf("Observed = (%v, %v), want the TLS tunnel to establish", attempted, established)
	}
	attempt.collect()
	elements := ledger.snapshot()
	if len(elements) != 1 {
		t.Fatalf("push_connections = %+v, want one rtmps session element", elements)
	}
	element := elements[0]
	if element.Family != "rtmps" || element.Purpose != "session" || element.Result != pushResultConnected {
		t.Fatalf("element = %+v, want a connected rtmps session", element)
	}
	if want := net.JoinHostPort(addr.String(), strconv.Itoa(receiver.port())); element.Target != want {
		t.Fatalf("element target = %q, want the judged address %q", element.Target, want)
	}
}

// bufferedConn keeps bytes the CONNECT head reader already buffered.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// TestJudgedProxyTLSHandshakeFailureIsUnreachable covers the counterexample on
// the judged proxy: the receiver accepts TCP and then closes without a
// handshake, so the dial alone must not be reported as a connection.
func TestJudgedProxyTLSHandshakeFailureIsUnreachable(t *testing.T) {
	addr := netip.MustParseAddr("127.0.0.1")
	raw, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	go func() {
		for {
			conn, err := raw.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	port := raw.Addr().(*net.TCPAddr).Port

	proxy, err := startJudgedHTTPProxy([]netip.Addr{addr}, "stream.example.test:"+strconv.Itoa(port), newPushLedger())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	proxy.BeginAttempt()
	proxy.SetAttemptTarget(net.JoinHostPort(addr.String(), strconv.Itoa(port)))
	proxy.SetAttemptTLS(true)

	conn, _, _ := dialCONNECT(t, proxy.URL(), "stream.example.test:"+strconv.Itoa(port))
	defer conn.Close()
	// The receiver closed right after the dial, so no handshake can complete.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	_, _ = conn.Read(buf)

	if attempted, established := proxy.Observed(); !attempted || established {
		t.Fatalf("Observed = (%v, %v), want attempted-but-not-established after the refused handshake", attempted, established)
	}
}

// TestRequestProxyRefusalIsRecordedUnreachable covers C-140 on the proxy the
// request named: the proxy accepts TCP and answers 407, so the element must be
// unreachable and the attempt must not open a second connection.
func TestRequestProxyRefusalIsRecordedUnreachable(t *testing.T) {
	addr := netip.MustParseAddr("127.0.0.1")
	refusing, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer refusing.Close()
	var mu sync.Mutex
	accepted := 0
	go func() {
		for {
			conn, err := refusing.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
			go func(conn net.Conn) {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
				time.Sleep(200 * time.Millisecond)
			}(conn)
		}
	}()

	out := jobOutput{target: "https://stream.example.test:8443/live"}
	args := []string{"-i", "<INPUT_FILE>", "-http_proxy", "http://" + refusing.Addr().String(), out.target}
	ledger := newPushLedger()
	var w Worker
	attempt, err := w.openPushAttempt(out, pushTransportTCP, addr, nil, ledger, args)
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.close()

	relayURL := ""
	if attempt.rewriteArgs != nil {
		for i, arg := range attempt.rewriteArgs(args) {
			if arg == "-http_proxy" && i+1 < len(args) {
				relayURL = attempt.rewriteArgs(args)[i+1]
			}
		}
	}
	if relayURL == "" {
		t.Fatal("the request proxy was not repointed at its relay")
	}
	conn, _, status := dialCONNECT(t, relayURL, "stream.example.test:8443")
	defer conn.Close()
	if !strings.Contains(status, "407") {
		t.Fatalf("relay response = %q, want the proxy's 407", status)
	}
	attempt.collect()

	mu.Lock()
	connections := accepted
	mu.Unlock()
	if connections != 1 {
		t.Fatalf("the refusing proxy accepted %d connection(s), want exactly 1", connections)
	}
	elements := ledger.snapshot()
	if len(elements) != 1 {
		t.Fatalf("push_connections = %+v, want one element", elements)
	}
	if elements[0].Result != pushResultUnreachable {
		t.Fatalf("element = %+v, want unreachable: a proxy that refuses the tunnel is not a connection", elements[0])
	}
	if elements[0].Target != refusing.Addr().String() {
		t.Fatalf("element target = %q, want the request proxy %q", elements[0].Target, refusing.Addr().String())
	}
}
