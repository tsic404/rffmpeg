package worker

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newJudgedProxyClient(t *testing.T, proxy *judgedHTTPProxy) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 10 * time.Second,
	}
}

// TestJudgedHTTPProxyBlocksRedirectTarget proves an HTTP-tunnel receiver cannot
// move a later connection outside the judged set by answering with a redirect.
func TestJudgedHTTPProxyBlocksRedirectTarget(t *testing.T) {
	trap, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 is unavailable: %v", err)
	}
	defer trap.Close()
	var trapAccepts atomic.Int64
	go func() {
		for {
			conn, err := trap.Accept()
			if err != nil {
				return
			}
			trapAccepts.Add(1)
			conn.Close()
		}
	}()

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open/1" {
			http.Redirect(w, r, "http://"+trap.Addr().String()+"/open/1", http.StatusTemporaryRedirect)
			return
		}
		io.WriteString(w, "open")
	}))
	defer receiver.Close()

	proxy, err := startJudgedHTTPProxy([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, receiver.Listener.Addr().String(), newPushLedger())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	client := newJudgedProxyClient(t, proxy)

	resp, err := client.Get(receiver.URL + "/open/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("judged request status = %d, want 307", resp.StatusCode)
	}

	resp, err = client.Get("http://" + trap.Addr().String() + "/open/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("redirect target status = %d, want 403", resp.StatusCode)
	}
	if got := trapAccepts.Load(); got != 0 {
		t.Errorf("redirect target accepted %d connection(s), want 0", got)
	}
}

// TestJudgedHTTPProxyKeepsRequestHostAndPath proves the proxy separates the
// receiver's virtual host and request path from the judged socket address.
func TestJudgedHTTPProxyKeepsRequestHostAndPath(t *testing.T) {
	const requestHost = "stream.example.test:1935"
	const requestURI = "/open/%2Fcolon:1?x=%25"
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != requestHost {
			t.Errorf("receiver Host = %q, want %q", r.Host, requestHost)
		}
		if r.RequestURI != requestURI {
			t.Errorf("receiver RequestURI = %q, want %q", r.RequestURI, requestURI)
		}
		io.WriteString(w, "forwarded")
	}))
	defer receiver.Close()

	proxy, err := startJudgedHTTPProxy([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, requestHost, newPushLedger())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	resp, err := newJudgedProxyClient(t, proxy).Get(receiver.URL + requestURI)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "forwarded" {
		t.Errorf("proxy response = %d %q, want 200 forwarded", resp.StatusCode, body)
	}
}

// TestJudgedHTTPProxyPinsAttemptTarget proves the proxy dials the judged
// endpoint chosen for this attempt while the request URL, Host and TLS name
// stay the request's own host. This is what lets HTTPS keep SNI and RTMPT keep
// its virtual host on a specific judged address.
func TestJudgedHTTPProxyPinsAttemptTarget(t *testing.T) {
	var requestHost string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != requestHost {
			t.Errorf("receiver Host = %q, want %q", r.Host, requestHost)
		}
		if r.URL.Path != "/live" {
			t.Errorf("receiver path = %q, want /live", r.URL.Path)
		}
		io.WriteString(w, "pinned")
	}))
	defer receiver.Close()
	requestHost = fmt.Sprintf("stream.example.test:%d", receiver.Listener.Addr().(*net.TCPAddr).Port)

	proxy, err := startJudgedHTTPProxy([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, requestHost, newPushLedger())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	proxy.BeginAttempt()
	proxy.SetAttemptTarget(receiver.Listener.Addr().String())
	resp, err := newJudgedProxyClient(t, proxy).Get("http://" + requestHost + "/live")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "pinned" {
		t.Errorf("proxy response = %d %q, want 200 pinned", resp.StatusCode, body)
	}
	attempted, established := proxy.Observed()
	if !attempted || !established {
		t.Fatalf("Observed = (%v, %v), want the pinned dial to establish", attempted, established)
	}
}

// TestJudgedHTTPProxyRejectsAttemptTargetOutsideSet proves the pin cannot
// smuggle an address the judgement never returned.
func TestJudgedHTTPProxyRejectsAttemptTargetOutsideSet(t *testing.T) {
	outside, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 is unavailable: %v", err)
	}
	defer outside.Close()
	var accepts atomic.Int64
	go func() {
		for {
			conn, err := outside.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			conn.Close()
		}
	}()

	proxy, err := startJudgedHTTPProxy([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, "stream.example.test:8443", newPushLedger())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	proxy.BeginAttempt()
	proxy.SetAttemptTarget(outside.Addr().String())

	resp, err := newJudgedProxyClient(t, proxy).Get("http://stream.example.test:8443/live")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("out-of-set attempt target status = %d, want 403", resp.StatusCode)
	}
	if got := accepts.Load(); got != 0 {
		t.Errorf("out-of-set attempt target accepted %d connection(s), want 0", got)
	}
}

// TestJudgedHTTPProxyTracksUpstreamDial proves the worker can attribute an
// unusable judged address without reading ffmpeg's report text.
func TestJudgedHTTPProxyTracksUpstreamDial(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer receiver.Close()

	proxy, err := startJudgedHTTPProxy([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, "stream.example.test", newPushLedger())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	client := newJudgedProxyClient(t, proxy)

	proxy.BeginAttempt()
	resp, err := client.Get("http://127.0.0.1:1/open/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unreachable judged address status = %d, want 502", resp.StatusCode)
	}
	if attempted, established := proxy.Observed(); !attempted || established {
		t.Error("a failed judged dial must be visible as no upstream connection")
	}

	proxy.BeginAttempt()
	resp, err = client.Get(receiver.URL + "/open/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reachable judged address status = %d, want 200", resp.StatusCode)
	}
	if attempted, established := proxy.Observed(); !attempted || !established {
		t.Error("a successful judged dial must not be reported as no upstream connection")
	}
}

// TestJudgedHTTPProxyCloseClosesHeldConnectTunnel reproduces a client and
// receiver that both keep a CONNECT tunnel open. Closing the proxy must close
// both sockets and let the copy paths exit.
func TestJudgedHTTPProxyCloseClosesHeldConnectTunnel(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	serverClosed := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			serverClosed <- err
			return
		}
		defer conn.Close()
		buf := make([]byte, 1)
		_, err = conn.Read(buf)
		serverClosed <- err
	}()

	proxy, err := startJudgedHTTPProxy([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, "", newPushLedger())
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		upstream.Addr(), upstream.Addr()); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	closed := make(chan struct{})
	go func() {
		proxy.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy.Close did not return while a CONNECT tunnel was held open")
	}

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := client.Read(buf); err == nil {
		t.Error("the local CONNECT client stayed open after the proxy closed")
	}
	select {
	case err := <-serverClosed:
		if err == nil {
			t.Error("the upstream side stayed open after the proxy closed")
		}
	case <-time.After(2 * time.Second):
		t.Error("the upstream side did not observe the proxy close")
	}
	t.Logf("upstream_accepts=1 tunnel_bytes=0 client_closed=true upstream_closed=true")
}

// TestClearedPushProxyEnvNeutralisesEveryInheritedProxyName proves a push
// execution starts with no proxy configuration of its own: a worker deployed
// behind a proxy must not let ffmpeg open a connection outside the judged set,
// and both spellings have to be cleared because libc reads the name literally
// (C-140).
func TestClearedPushProxyEnvNeutralisesEveryInheritedProxyName(t *testing.T) {
	env := clearedPushProxyEnv()
	got := make(map[string]bool, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value != "" {
			t.Fatalf("env entry %q is not a cleared key", entry)
		}
		got[key] = true
	}
	for _, name := range []string{"http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		if !got[name] || !got[strings.ToUpper(name)] {
			t.Errorf("env = %v, want both spellings of %s cleared", env, name)
		}
	}
}

// TestJudgedHTTPProxyRecordsGuidanceOnlyOutsideTheSet covers what a redirect
// contributes to the connection list: a target outside the judged set is a
// server-guided blocked element, while a relative Location or the request's own
// name stays in this attempt and adds nothing (C-130, C-134).
func TestJudgedHTTPProxyRecordsGuidanceOnlyOutsideTheSet(t *testing.T) {
	ledger := newPushLedger()
	proxy := &judgedHTTPProxy{
		allowed: map[netip.Addr]struct{}{netip.MustParseAddr("10.77.0.2"): {}},
		host:    "origin.example.test:8080",
		ledger:  ledger,
	}

	for _, location := range []string{
		"/relative/path",
		"../up",
		"http://origin.example.test:8080/again",
		"http://10.77.0.2:8080/again",
	} {
		proxy.recordGuidance(location)
	}
	if got := ledger.snapshot(); len(got) != 0 {
		t.Fatalf("in-set guidance recorded %+v, want nothing", got)
	}

	proxy.recordGuidance("https://elsewhere.example.test:8443/next")
	got := ledger.snapshot()
	if len(got) != 1 {
		t.Fatalf("out-of-set guidance recorded %+v, want one element", got)
	}
	element := got[0]
	if element.Result != "blocked" || element.Origin != "server" {
		t.Fatalf("element = %+v, want blocked with origin server", element)
	}
	if element.Family != "https" || element.Purpose != "session" {
		t.Fatalf("element = %+v, want an https session family", element)
	}
	if element.Target != "elsewhere.example.test:8443" {
		t.Fatalf("element target = %q, want the guided address", element.Target)
	}
}
