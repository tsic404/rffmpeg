package worker

import (
	"context"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// pushTransport is how a push is kept on the judged address set and how the
// worker observes the connection attempt.
type pushTransport int

const (
	// pushTransportTCP forwards a byte stream through a loopback listener.
	pushTransportTCP pushTransport = iota
	// pushTransportUDP forwards datagrams through a loopback listener.
	pushTransportUDP
	// pushTransportProxy routes HTTP-shaped protocols through the judged HTTP
	// forward proxy, which can carry the request host name separately.
	pushTransportProxy
	// pushTransportRTSP relays the RTSP control connection and rewrites the
	// SETUP response so negotiated RTP/RTCP lands on the judged IP literal.
	pushTransportRTSP
	// pushTransportDirect hands ffmpeg the judged address itself. It is used
	// only where an outbound relay cannot represent the protocol (an SRT
	// listener binds instead of dialing), so it has no fallback observation.
	pushTransportDirect
)

// pushDialObserver is the worker-owned evidence of one push attempt: whether a
// dial to the judged address was made and whether it established. It replaces
// any inference from ffmpeg's exit status, progress stream or log text.
type pushDialObserver interface {
	BeginAttempt()
	Observed() (attempted, established bool)
}

func pushTargetScheme(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Scheme)
}

// pushTransportFor classifies a protocol by how its connection leaves ffmpeg.
// TLS and the RTMPT tunnel need the forward proxy because it carries the
// request host name separately from the judged socket; UDP-carried protocols
// need a datagram relay; every other protocol is a byte stream over TCP.
func pushTransportFor(target string) pushTransport {
	parsed, err := url.Parse(target)
	if err != nil {
		return pushTransportTCP
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch {
	case scheme == "https":
		// TLS separates the verification/SNI name from the socket address;
		// the judged proxy keeps the request URL (and therefore SNI) while it
		// CONNECTs to the attempt's judged address.
		return pushTransportProxy
	case strings.HasPrefix(scheme, "rtmpt"):
		return pushTransportProxy
	case scheme == "rtsp":
		return pushTransportRTSP
	case scheme == "udp", scheme == "rtp", scheme == "rtp_mpegts":
		return pushTransportUDP
	case scheme == "srt":
		if strings.EqualFold(parsed.Query().Get("mode"), "listener") {
			return pushTransportDirect
		}
		return pushTransportUDP
	default:
		return pushTransportTCP
	}
}

// shouldRetryPush is the only address-fallback decision: the worker itself saw
// that this attempt dialed the judged address and no dial established. A dial
// that established, even if the protocol failed later, is not retried.
func shouldRetryPush(observer pushDialObserver) bool {
	attempted, established := observer.Observed()
	return attempted && !established
}

// judgedTCPRelay is a job-local TCP relay. FFmpeg connects to the loopback
// listener; the relay dials the judged address and forwards the bytes. The
// receiver's only connection is that dial, so a single-accept receiver does
// not lose its accept to a selection probe.
type judgedTCPRelay struct {
	listener net.Listener
	judged   string

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	conns   map[net.Conn]struct{}
	dialing int
	closed  bool
	// draining ends the accept loop after ffmpeg exits so Observed sees every
	// connection ffmpeg already made. A connection still in the kernel queue
	// must be counted before the address-fallback decision.
	draining   bool
	acceptDone chan struct{}
	work       sync.WaitGroup

	dials       atomic.Int64
	established atomic.Int64
	forwarded   atomic.Int64

	// rewriter, when set, rewrites the upstream byte stream. It is immutable
	// after the relay starts.
	rewriter *rtspTransportRewriter
	// dialSucceeded is a test hook run after a dial succeeds but before the
	// result is published. It is injected at construction and never written
	// afterwards; production leaves it nil.
	dialSucceeded func()
}

func startJudgedTCPRelay(judged string) (*judgedTCPRelay, error) {
	return startJudgedTCPRelayWith(judged, nil, nil)
}

func startJudgedTCPRelayWith(judged string, rewriter *rtspTransportRewriter, dialSucceeded func()) (*judgedTCPRelay, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	relay := &judgedTCPRelay{
		listener:      listener,
		judged:        judged,
		ctx:           ctx,
		cancel:        cancel,
		conns:         make(map[net.Conn]struct{}),
		acceptDone:    make(chan struct{}),
		rewriter:      rewriter,
		dialSucceeded: dialSucceeded,
	}
	relay.cond = sync.NewCond(&relay.mu)
	go relay.accept()
	return relay, nil
}

// startJudgedRTSPRelay fronts an RTSP control connection. The relay dials the
// judged address and rewrites SETUP replies so ffmpeg's RTP/RTCP destination is
// that same judged IP literal, never the loopback peer of the control socket.
func startJudgedRTSPRelay(judged string, judgedAddr netip.Addr) (*judgedTCPRelay, error) {
	return startJudgedTCPRelayWith(judged, newRTSPTransportRewriter(judgedAddr), nil)
}

// Addr is the loopback address ffmpeg connects to.
func (r *judgedTCPRelay) Addr() string {
	return r.listener.Addr().String()
}

func (r *judgedTCPRelay) accept() {
	defer close(r.acceptDone)
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				r.mu.Lock()
				draining := r.draining
				r.mu.Unlock()
				if draining {
					return
				}
				continue
			}
			return
		}
		if !r.track(conn) {
			_ = conn.Close()
			return
		}
		r.mu.Lock()
		r.dialing++
		r.mu.Unlock()
		r.work.Add(1)
		go func() {
			defer r.work.Done()
			r.handle(conn)
		}()
	}
}

func (r *judgedTCPRelay) handle(client net.Conn) {
	defer r.untrack(client)
	defer client.Close()

	r.dials.Add(1)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	upstream, err := dialer.DialContext(r.ctx, "tcp", r.judged)

	if err == nil && r.dialSucceeded != nil {
		r.dialSucceeded()
	}
	// Publish the dial result before the in-flight count drops. Observed reads
	// both under the same lock after waiting for dialing==0, so it can never
	// see "dialed but not established" for a connection that did establish.
	r.mu.Lock()
	if err == nil {
		r.established.Add(1)
	}
	r.dialing--
	r.cond.Broadcast()
	r.mu.Unlock()

	if err != nil {
		return
	}
	if !r.track(upstream) {
		_ = upstream.Close()
		return
	}
	defer r.untrack(upstream)
	defer upstream.Close()

	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		n, err := io.Copy(upstream, client)
		r.forwarded.Add(n)
		if err != nil {
			_ = upstream.Close()
			return
		}
		closeWrite(upstream)
	}()
	go func() {
		defer copies.Done()
		if r.rewriter == nil {
			_, err := io.Copy(client, upstream)
			if err != nil {
				_ = client.Close()
				return
			}
			closeWrite(client)
			return
		}
		buf := make([]byte, 32*1024)
		for {
			n, readErr := upstream.Read(buf)
			if n > 0 {
				if out := r.rewriter.Rewrite(buf[:n]); len(out) > 0 {
					if _, writeErr := client.Write(out); writeErr != nil {
						_ = upstream.Close()
						return
					}
				}
			}
			if readErr != nil {
				if out := r.rewriter.Flush(); len(out) > 0 {
					_, _ = client.Write(out)
				}
				closeWrite(client)
				return
			}
		}
	}()
	copies.Wait()
}

// closeWrite ends one direction of a forwarded connection without discarding
// data the peer has not read yet.
func closeWrite(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
		return
	}
	_ = conn.Close()
}

func (r *judgedTCPRelay) track(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.conns[conn] = struct{}{}
	return true
}

func (r *judgedTCPRelay) untrack(conn net.Conn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
}

func (r *judgedTCPRelay) BeginAttempt() {
	r.dials.Store(0)
	r.established.Store(0)
	r.forwarded.Store(0)
}

// Observed finalizes the attempt: it drains the accept queue, waits for every
// accepted connection's dial to finish, and only then reports. Draining is what
// makes a connection ffmpeg made just before exiting visible instead of leaving
// it unaccepted and uncounted.
func (r *judgedTCPRelay) Observed() (bool, bool) {
	r.mu.Lock()
	if !r.draining {
		r.draining = true
		if tcp, ok := r.listener.(*net.TCPListener); ok {
			_ = tcp.SetDeadline(time.Now().Add(50 * time.Millisecond))
		}
	}
	r.mu.Unlock()

	<-r.acceptDone

	r.mu.Lock()
	defer r.mu.Unlock()
	for r.dialing > 0 {
		r.cond.Wait()
	}
	return r.dials.Load() > 0, r.established.Load() > 0
}

func (r *judgedTCPRelay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.work.Wait()
		return
	}
	r.closed = true
	conns := make([]net.Conn, 0, len(r.conns))
	for conn := range r.conns {
		conns = append(conns, conn)
	}
	r.conns = nil
	r.cond.Broadcast()
	r.mu.Unlock()

	r.cancel()
	_ = r.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	r.work.Wait()
}

// judgedUDPRelay is the datagram counterpart. SRT's caller handshake makes an
// upstream response the establishment fact; a one-way UDP protocol treats the
// first forwarded datagram as use of that address and is never retried.
type judgedUDPRelay struct {
	local     *net.UDPConn
	upstream  *net.UDPConn
	handshake bool

	mu     sync.Mutex
	client *net.UDPAddr
	closed bool

	sent     atomic.Int64
	received atomic.Int64

	work    sync.WaitGroup
	drained chan struct{}
}

// udpDrainWindow is how long Close keeps reading after ffmpeg exits, so
// datagrams it already wrote into the loopback socket are still forwarded to
// the judged address instead of being discarded with the socket.
const udpDrainWindow = 100 * time.Millisecond

func startJudgedUDPRelay(judged string, handshake bool) (*judgedUDPRelay, error) {
	local, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	upstreamAddr, err := net.ResolveUDPAddr("udp", judged)
	if err != nil {
		_ = local.Close()
		return nil, err
	}
	upstream, err := net.DialUDP("udp", nil, upstreamAddr)
	if err != nil {
		_ = local.Close()
		return nil, err
	}
	// The sender bursts a whole segment; a small kernel buffer would drop
	// datagrams before the forwarding goroutine reads them.
	_ = local.SetReadBuffer(4 << 20)
	_ = upstream.SetReadBuffer(1 << 20)
	relay := &judgedUDPRelay{
		local:     local,
		upstream:  upstream,
		handshake: handshake,
		drained:   make(chan struct{}),
	}
	relay.work.Add(2)
	go func() {
		defer relay.work.Done()
		relay.forwardToJudged()
	}()
	go func() {
		defer relay.work.Done()
		relay.forwardToFFmpeg()
	}()
	return relay, nil
}

// Addr is the loopback datagram address ffmpeg sends to.
func (r *judgedUDPRelay) Addr() string {
	return r.local.LocalAddr().String()
}

func (r *judgedUDPRelay) forwardToJudged() {
	defer close(r.drained)
	buf := make([]byte, 65535)
	for {
		n, client, err := r.local.ReadFromUDP(buf)
		if err != nil {
			return
		}
		r.mu.Lock()
		r.client = client
		r.mu.Unlock()
		r.sent.Add(int64(n))
		if _, err := r.upstream.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (r *judgedUDPRelay) forwardToFFmpeg() {
	buf := make([]byte, 65535)
	for {
		n, err := r.upstream.Read(buf)
		if err != nil {
			return
		}
		r.received.Add(int64(n))
		r.mu.Lock()
		client := r.client
		r.mu.Unlock()
		if client != nil {
			_, _ = r.local.WriteToUDP(buf[:n], client)
		}
	}
}

func (r *judgedUDPRelay) BeginAttempt() {
	r.sent.Store(0)
	r.received.Store(0)
}

func (r *judgedUDPRelay) Observed() (bool, bool) {
	r.finishAttempt()
	sent := r.sent.Load()
	if !r.handshake {
		return sent > 0, sent > 0
	}
	return sent > 0, r.received.Load() > 0
}

// finishAttempt stops both directions after ffmpeg exits and reports only
// after every datagram it queued has been forwarded and counted. Draining
// before the address decision keeps the tail of an attempt on the address
// that produced it, so a fallback cannot re-send the same media.
func (r *judgedUDPRelay) finishAttempt() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.work.Wait()
		return
	}
	r.closed = true
	r.mu.Unlock()

	// ffmpeg has exited; keep the local socket open long enough to consume
	// what it already queued, then stop the upstream side. Closing both
	// sockets immediately would silently drop the tail of the media.
	_ = r.local.SetReadDeadline(time.Now().Add(udpDrainWindow))
	<-r.drained
	_ = r.upstream.SetReadDeadline(time.Now().Add(udpDrainWindow))
	r.work.Wait()
	_ = r.upstream.Close()
}

func (r *judgedUDPRelay) Close() {
	r.finishAttempt()
	_ = r.local.Close()
}

// judgedRTPRelays fronts both UDP ports of the RTP protocol: media goes to the
// URL port and RTCP goes to the explicit rtcpport or the URL port+1. FFmpeg
// opens them as independent sockets, so one relay cannot represent the
// protocol.
type judgedRTPRelays struct {
	rtp  *judgedUDPRelay
	rtcp *judgedUDPRelay
}

func startJudgedRTPRelays(judgedHost string, rtpPort, rtcpPort int) (*judgedRTPRelays, error) {
	rtp, err := startJudgedUDPRelay(net.JoinHostPort(judgedHost, strconv.Itoa(rtpPort)), false)
	if err != nil {
		return nil, err
	}
	rtcp, err := startJudgedUDPRelay(net.JoinHostPort(judgedHost, strconv.Itoa(rtcpPort)), false)
	if err != nil {
		rtp.Close()
		return nil, err
	}
	return &judgedRTPRelays{rtp: rtp, rtcp: rtcp}, nil
}

func (r *judgedRTPRelays) BeginAttempt() {
	r.rtp.BeginAttempt()
	r.rtcp.BeginAttempt()
}

func (r *judgedRTPRelays) Observed() (bool, bool) {
	rtpAttempted, rtpEstablished := r.rtp.Observed()
	rtcpAttempted, rtcpEstablished := r.rtcp.Observed()
	return rtpAttempted || rtcpAttempted, rtpEstablished || rtcpEstablished
}

func (r *judgedRTPRelays) Close() {
	r.rtp.Close()
	r.rtcp.Close()
}
