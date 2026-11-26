package worker

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errProxyTargetDenied = errors.New("target is outside the judged address set")

// judgedHTTPProxy is a job-local forward proxy for HTTP-tunneled protocols.
// FFmpeg's RTMPT implementation creates an internal HTTP context with default
// options, so a top-level max_redirects cannot constrain it. Routing that
// context through this proxy keeps every request on the judged address set:
// the proxy refuses a connection before it is attempted, including one caused
// by a redirect.
type judgedHTTPProxy struct {
	listener  net.Listener
	server    *http.Server
	transport *http.Transport
	allowed   map[netip.Addr]struct{}
	host      string

	ctx       context.Context
	cancel    context.CancelFunc
	serveDone chan struct{}

	mu      sync.Mutex
	closed  bool
	tunnels map[*proxyTunnel]struct{}
	work    sync.WaitGroup
	// attemptTarget is the judged host:port this attempt must dial. The
	// request URL keeps its host name, so TLS SNI, certificate verification
	// and HTTP Host still see the name the request used.
	attemptTarget string
	// attemptTLS marks a target whose protocol runs TLS on the tunnelled
	// connection, so a CONNECT that never completed the handshake is not a
	// connection (C-140, C-142).
	attemptTLS bool

	attemptDials       atomic.Int64
	attemptEstablished atomic.Int64

	// ledger records server-guided redirects that leave the judged set, so the
	// job's connection list carries them as blocked elements (C-130).
	ledger *pushLedger
}

type proxyTunnel struct {
	client   net.Conn
	upstream net.Conn
}

func (t *proxyTunnel) close() {
	_ = t.client.Close()
	_ = t.upstream.Close()
}

func startJudgedHTTPProxy(allowed []netip.Addr, requestHost string, ledger *pushLedger) (*judgedHTTPProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &judgedHTTPProxy{
		listener:  listener,
		allowed:   make(map[netip.Addr]struct{}, len(allowed)),
		host:      requestHost,
		ctx:       ctx,
		cancel:    cancel,
		serveDone: make(chan struct{}),
		tunnels:   make(map[*proxyTunnel]struct{}),
		ledger:    ledger,
	}
	for _, addr := range allowed {
		proxy.allowed[addr.Unmap()] = struct{}{}
	}
	proxy.transport = &http.Transport{
		Proxy:              nil,
		DialContext:        proxy.dialJudged,
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
		MaxIdleConns:       4,
		IdleConnTimeout:    30 * time.Second,
	}
	proxy.server = &http.Server{
		Handler:           http.HandlerFunc(proxy.handle),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		defer close(proxy.serveDone)
		_ = proxy.server.Serve(listener)
	}()
	return proxy, nil
}

func (p *judgedHTTPProxy) URL() string {
	return "http://" + p.listener.Addr().String()
}

// BeginAttempt starts a fresh structured dial observation for one ffmpeg
// invocation. The worker reads it after the process exits to decide whether
// the judged address was reachable at all.
func (p *judgedHTTPProxy) BeginAttempt() {
	p.attemptDials.Store(0)
	p.attemptEstablished.Store(0)
	p.mu.Lock()
	p.attemptTarget = ""
	p.attemptTLS = false
	p.mu.Unlock()
}

// SetAttemptTarget pins this attempt's judged endpoint. The request host stays
// the name from the target, so every protocol-level name remains the request's.
func (p *judgedHTTPProxy) SetAttemptTarget(target string) {
	p.mu.Lock()
	p.attemptTarget = target
	p.mu.Unlock()
}

// SetAttemptTLS records whether this attempt's protocol runs TLS inside the
// tunnel, which makes the handshake part of the connection evidence.
func (p *judgedHTTPProxy) SetAttemptTLS(tls bool) {
	p.mu.Lock()
	p.attemptTLS = tls
	p.mu.Unlock()
}

// Observed reports the worker-owned connection evidence for the current
// attempt: whether the proxy dialed any judged upstream and whether one
// established. A dial to the receiver only proves the receiver's TCP port
// answered; the channel the protocol needs (the CONNECT tunnel, and its TLS
// handshake where the protocol has one) has to complete as well (C-140,
// C-142). It first waits for in-flight requests, so a tunnel still finishing
// cannot be mistaken for a pre-connection failure.
func (p *judgedHTTPProxy) Observed() (bool, bool) {
	p.work.Wait()
	return p.attemptDials.Load() > 0, p.attemptEstablished.Load() > 0
}

func (p *judgedHTTPProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.work.Wait()
		<-p.serveDone
		return
	}
	p.closed = true
	p.cancel()
	tunnels := make([]*proxyTunnel, 0, len(p.tunnels))
	for tunnel := range p.tunnels {
		tunnels = append(tunnels, tunnel)
	}
	p.tunnels = nil
	p.mu.Unlock()
	_ = p.server.Close()
	for _, tunnel := range tunnels {
		tunnel.close()
	}
	p.transport.CloseIdleConnections()
	p.work.Wait()
	<-p.serveDone
}

func (p *judgedHTTPProxy) handle(w http.ResponseWriter, r *http.Request) {
	if !p.beginWork() {
		http.Error(w, "proxy is closing", http.StatusServiceUnavailable)
		return
	}
	defer p.work.Done()
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "proxy requires an absolute URI", http.StatusBadRequest)
		return
	}
	if strings.ToLower(r.URL.Scheme) != "http" {
		http.Error(w, "proxy supports only http requests and CONNECT", http.StatusBadRequest)
		return
	}
	target, err := p.resolveJudged(r.URL.Host)
	if err != nil {
		http.Error(w, "target outside the judged address set", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()

	out := r.Clone(ctx)
	out.RequestURI = ""
	// Dial the validated literal. The request hostname is carried separately
	// as Host, so a receiver that routes by domain still sees the name the
	// request used while the socket lands on the judged address.
	out.URL.Scheme = "http"
	out.URL.Host = target
	if p.host != "" {
		out.Host = p.host
	}
	response, err := p.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "target request failed", http.StatusBadGateway)
		return
	}
	// A plain proxied request established once the receiver answered it; the
	// proxy's own 502 above is the only failure this path can produce.
	p.attemptEstablished.Add(1)
	defer response.Body.Close()
	// A redirect is the server's guidance to another target. When that target
	// is outside the judged set the job's list records it as blocked, so the
	// address is judged unusable instead of the guidance being followed
	// (C-130, C-137).
	if location := response.Header.Get("Location"); response.StatusCode >= 300 && response.StatusCode <= 399 && location != "" {
		p.recordGuidance(location)
	}
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (p *judgedHTTPProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target, err := p.resolveJudged(r.Host)
	if err != nil {
		http.Error(w, "target outside the judged address set", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	upstream, err := p.dialTarget(ctx, target)
	if err != nil {
		http.Error(w, "target connection failed", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "proxy cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	tunnel := &proxyTunnel{client: client, upstream: upstream}
	if !p.trackTunnel(tunnel) {
		tunnel.close()
		return
	}
	defer p.untrackTunnel(tunnel)
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// The 200 above is this proxy's own answer, so the channel to the receiver
	// is open; what remains unknown is whether the protocol's TLS handshake
	// completed inside it. The observer reads both directions and decides.
	observer := newEstablishedTunnelObserver(p.attemptTunnelTLS())

	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		_, _ = forwardObserved(upstream, client, observer.ClientBytes)
		_ = upstream.Close()
	}()
	go func() {
		defer copies.Done()
		_, _ = forwardObserved(client, upstream, observer.ServerBytes)
		_ = client.Close()
	}()
	copies.Wait()
	if observer.Established() {
		p.attemptEstablished.Add(1)
	}
}

func (p *judgedHTTPProxy) dialJudged(ctx context.Context, network, address string) (net.Conn, error) {
	target, err := p.resolveJudged(address)
	if err != nil {
		return nil, err
	}
	return p.dialTarget(ctx, target)
}

func (p *judgedHTTPProxy) dialTarget(ctx context.Context, target string) (net.Conn, error) {
	p.attemptDials.Add(1)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return dialer.DialContext(ctx, "tcp", target)
}

func (p *judgedHTTPProxy) attemptTunnelTLS() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attemptTLS
}

// recordGuidance adds a blocked element when a redirect names a target outside
// the judged set. A relative Location is same-host and needs no element.
func (p *judgedHTTPProxy) recordGuidance(location string) {
	parsed, err := url.Parse(location)
	if err != nil || parsed.Host == "" {
		return
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		host, port = strings.Trim(parsed.Host, "[]"), "80"
		if strings.EqualFold(parsed.Scheme, "https") {
			port = "443"
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if _, ok := p.allowed[addr.Unmap()]; ok {
			return
		}
	} else {
		// After the address decision a name is not resolved again. Only the
		// request's own host maps back to this attempt's judged address; any
		// other name is outside the judged set (C-130, C-133).
		requestHost, _, splitErr := net.SplitHostPort(p.host)
		if splitErr != nil {
			requestHost = strings.Trim(p.host, "[]")
		}
		if strings.EqualFold(host, requestHost) {
			return
		}
	}
	if p.ledger == nil {
		return
	}
	p.ledger.recordBlocked(
		pushConnectionKind{family: redirectFamily(parsed.Scheme), purpose: "session"},
		net.JoinHostPort(host, port), pushOriginServer)
}

// redirectFamily is the C-141 session family a redirect target's scheme names.
// The list has no family for an unknown scheme, so only the schemes the table
// lists keep their own name and everything else falls back to the transport
// the request itself used.
func redirectFamily(scheme string) string {
	if kind, ok := pushSessionKind(strings.ToLower(scheme)); ok {
		return kind.family
	}
	return "http"
}

func (p *judgedHTTPProxy) beginWork() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.work.Add(1)
	return true
}

func (p *judgedHTTPProxy) trackTunnel(tunnel *proxyTunnel) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.tunnels[tunnel] = struct{}{}
	return true
}

func (p *judgedHTTPProxy) untrackTunnel(tunnel *proxyTunnel) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tunnels, tunnel)
}

// resolveJudged maps a host:port to one literal judged address. A host name is
// resolved once and every answer must be in the judged set; a caller never
// gets a second chance to resolve into a denied address.
func (p *judgedHTTPProxy) resolveJudged(hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = strings.Trim(hostport, "[]"), "80"
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if _, ok := p.allowed[addr]; !ok {
			return "", errProxyTargetDenied
		}
		return net.JoinHostPort(addr.String(), port), nil
	}
	if target := p.attemptJudgedTarget(); target != "" && strings.EqualFold(host, p.requestHostname()) {
		targetHost, _, splitErr := net.SplitHostPort(target)
		if splitErr != nil {
			targetHost = target
		}
		addr, parseErr := netip.ParseAddr(targetHost)
		if parseErr != nil {
			return "", errProxyTargetDenied
		}
		addr = addr.Unmap()
		if _, ok := p.allowed[addr]; !ok {
			return "", errProxyTargetDenied
		}
		return net.JoinHostPort(addr.String(), port), nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", host)
	if err != nil || len(addrs) == 0 {
		return "", errProxyTargetDenied
	}
	var first netip.Addr
	for _, addr := range addrs {
		addr = addr.Unmap()
		if _, ok := p.allowed[addr]; !ok {
			return "", errProxyTargetDenied
		}
		if !first.IsValid() {
			first = addr
		}
	}
	return net.JoinHostPort(first.String(), port), nil
}

func (p *judgedHTTPProxy) attemptJudgedTarget() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attemptTarget
}

// requestHostname is the host part of the target the job asked to push to.
func (p *judgedHTTPProxy) requestHostname() string {
	host, _, err := net.SplitHostPort(p.host)
	if err != nil {
		return strings.Trim(p.host, "[]")
	}
	return host
}
