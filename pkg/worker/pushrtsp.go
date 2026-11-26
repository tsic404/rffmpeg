package worker

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
)

// pushHeaderBudget bounds the response header a rewriter buffers before it has
// seen the blank line that ends it. A peer that never sends one would otherwise
// grow this buffer for as long as it stays connected, so exceeding the budget
// ends the attempt instead of the worker.
const pushHeaderBudget = 64 << 10

// rtspTransportRewriter rewrites SETUP replies on the upstream side of the
// RTSP control relay. ffmpeg sends UDP media to the `source` and `server_port`
// the reply names, so the reply is repointed at a local relay that dials the
// judged address. Interleaved TCP media stays on the judged control connection
// and records nothing (C-134).
type rtspTransportRewriter struct {
	judged      netip.Addr
	media       *rtspMediaRelays
	buf         []byte
	body        int
	interleaved int
	// dead is set once the peer exceeded the header budget. The relay then
	// stops forwarding and closes the connection, so one misbehaving receiver
	// costs this attempt and nothing else.
	dead bool
}

// Failed reports whether the rewriter gave up on this connection.
func (r *rtspTransportRewriter) Failed() bool { return r.dead }

func newRTSPTransportRewriter(judged netip.Addr, media *rtspMediaRelays) *rtspTransportRewriter {
	return &rtspTransportRewriter{judged: judged, media: media}
}

// rtspMediaRelays fronts the RTP/RTCP sockets a UDP RTSP session negotiates.
// One pair is opened per SETUP, and each pair reports its own `rtp` and `rtcp`
// derived elements (C-141).
type rtspMediaRelays struct {
	judged netip.Addr
	ledger *pushLedger
	mu     sync.Mutex
	pairs  []*rtspMediaPair
}

type rtspMediaPair struct {
	rtp  *judgedUDPRelay
	rtcp *judgedUDPRelay
}

// open starts the datagram pair for one SETUP and returns the loopback
// addresses the reply must name.
func (m *rtspMediaRelays) open(rtpPort, rtcpPort int) (string, string, bool) {
	rtp, err := startJudgedUDPRelay(net.JoinHostPort(m.judged.String(), strconv.Itoa(rtpPort)), false)
	if err != nil {
		return "", "", false
	}
	rtcp, err := startJudgedUDPRelay(net.JoinHostPort(m.judged.String(), strconv.Itoa(rtcpPort)), false)
	if err != nil {
		rtp.Close()
		return "", "", false
	}
	m.mu.Lock()
	m.pairs = append(m.pairs, &rtspMediaPair{rtp: rtp, rtcp: rtcp})
	m.mu.Unlock()
	return rtp.Addr(), rtcp.Addr(), true
}

// flows reports one `rtp` and one `rtcp` derived element per negotiated pair.
// A pair ffmpeg never sent on contributes nothing. Both ports come from the
// SETUP reply's `server_port`, so the elements carry the server as their
// origin rather than the request (C-130, C-134); a request-derived FEC or RTCP
// port keeps `request`.
func (m *rtspMediaRelays) flows() []pushDatagramFlow {
	m.mu.Lock()
	pairs := m.pairs
	m.mu.Unlock()
	var flows []pushDatagramFlow
	for _, pair := range pairs {
		if flow, ok := pair.rtp.flow("rtp", "derived"); ok {
			flow.Origin = pushOriginServer
			flows = append(flows, flow)
		}
		if flow, ok := pair.rtcp.flow("rtcp", "derived"); ok {
			flow.Origin = pushOriginServer
			flows = append(flows, flow)
		}
	}
	return flows
}

func (m *rtspMediaRelays) close() {
	m.mu.Lock()
	pairs := m.pairs
	m.pairs = nil
	m.mu.Unlock()
	for _, pair := range pairs {
		pair.rtp.Close()
		pair.rtcp.Close()
	}
}

// Rewrite consumes a chunk of the upstream byte stream and returns the bytes
// that may be forwarded. It buffers until a complete RTSP header, body or
// interleaved frame is available.
func (r *rtspTransportRewriter) Rewrite(chunk []byte) []byte {
	r.buf = append(r.buf, chunk...)
	var out []byte
	for len(r.buf) > 0 {
		if r.interleaved > 0 {
			n := r.interleaved
			if n > len(r.buf) {
				n = len(r.buf)
			}
			out = append(out, r.buf[:n]...)
			r.buf = r.buf[n:]
			r.interleaved -= n
			if r.interleaved > 0 {
				break
			}
			continue
		}
		if r.body > 0 {
			n := r.body
			if n > len(r.buf) {
				n = len(r.buf)
			}
			out = append(out, r.buf[:n]...)
			r.buf = r.buf[n:]
			r.body -= n
			if r.body > 0 {
				break
			}
			continue
		}
		if r.buf[0] == '$' {
			if len(r.buf) < 4 {
				break
			}
			// Interleaved frames can be large and may stall mid-frame; stream
			// the header and payload instead of buffering the whole frame.
			out = append(out, r.buf[:4]...)
			r.interleaved = int(binary.BigEndian.Uint16(r.buf[2:4]))
			r.buf = r.buf[4:]
			continue
		}
		end := bytes.Index(r.buf, []byte("\r\n\r\n"))
		if end < 0 {
			if len(r.buf) > pushHeaderBudget {
				r.dead = true
				r.buf = nil
			}
			break
		}
		header := r.buf[:end+4]
		r.buf = r.buf[end+4:]
		rewritten, body := r.rewriteHeader(header)
		out = append(out, rewritten...)
		r.body = body
	}
	return out
}

// Flush returns whatever remains when the upstream closes mid-message. A
// truncated message is forwarded unchanged rather than swallowed.
func (r *rtspTransportRewriter) Flush() []byte {
	out := r.buf
	r.buf = nil
	return out
}

func (r *rtspTransportRewriter) rewriteHeader(header []byte) ([]byte, int) {
	lines := strings.Split(string(header), "\r\n")
	if len(lines) == 0 {
		return header, 0
	}
	status := strings.Fields(lines[0])
	if len(status) < 2 || !strings.HasPrefix(status[0], "RTSP/") || status[1] != "200" {
		return header, contentLength(lines)
	}
	refused := false
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Transport") {
			kept = append(kept, line)
			continue
		}
		rewritten, block := r.rewriteTransport(strings.TrimSpace(value))
		if block {
			refused = true
			continue
		}
		kept = append(kept, "Transport: "+rewritten)
	}
	if refused {
		// A SETUP whose media destination lies outside the judged set must not
		// be honoured: the error status stops ffmpeg from sending media, and
		// dropping the Transport header leaves it no destination to fall back
		// on. The blocked elements recorded above make this address unusable
		// (C-130, C-137).
		kept = append([]string{"RTSP/1.0 461 Unsupported Transport"}, kept[1:]...)
	}
	return []byte(strings.Join(kept, "\r\n")), contentLength(kept)
}

func contentLength(lines []string) int {
	for _, line := range lines {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// rewriteTransport repoints a SETUP reply's media destination at the local
// relay pair. It reports refused when the server's guidance names a destination
// outside the judged set, in which case the caller turns the reply into an
// error and no media is sent.
func (r *rtspTransportRewriter) rewriteTransport(value string) (string, bool) {
	parts := strings.Split(value, ";")
	trimmed := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			trimmed = append(trimmed, part)
		}
	}
	if len(trimmed) == 0 {
		return value, false
	}
	transport, ok := parseRTSPTransport(trimmed[0])
	if !ok {
		// The media destination cannot be established, so nothing may be
		// forwarded: a reply whose transport this code cannot read is refused
		// rather than assumed to carry media on the control connection
		// (C-133, C-137).
		return value, true
	}
	// Interleaved TCP carries media on the judged control connection, and
	// multicast has no unicast destination to pin. Neither records a derived
	// element, and neither needs a relay.
	if transport == "tcp" {
		return value, false
	}
	var serverPorts, source string
	for _, part := range trimmed {
		if strings.EqualFold(part, "multicast") {
			return value, false
		}
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, "server_port=") {
			serverPorts = part[len("server_port="):]
		}
		if strings.HasPrefix(lower, "source=") {
			source = part[len("source="):]
		}
	}
	if serverPorts == "" {
		return value, false
	}
	rtpPort, rtcpPort, ok := splitServerPorts(serverPorts)
	if !ok {
		return value, false
	}
	// The reply's source is the server's guidance (C-130). A source outside
	// the judged set is never dialed: both derived elements are recorded as
	// blocked and the SETUP is refused.
	if source != "" && !r.guidanceInside(source) {
		r.recordBlocked(rtpPort, rtcpPort, source)
		return value, true
	}
	rtpLocal, rtcpLocal, ok := r.media.open(rtpPort, rtcpPort)
	if !ok {
		return value, false
	}
	kept := make([]string, 0, len(trimmed)+2)
	for _, part := range trimmed {
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, "server_port=") || strings.HasPrefix(lower, "source=") || strings.HasPrefix(lower, "destination=") {
			continue
		}
		kept = append(kept, part)
	}
	kept = append(kept, "server_port="+relayPortPair(rtpLocal, rtcpLocal), "source=127.0.0.1")
	return strings.Join(kept, ";"), false
}

// parseRTSPTransport reads the transport-protocol/profile/lower-transport
// token of a Transport header (RFC 2326 §12.39). A two-part spec omits the
// transport and defaults to UDP. Anything else — an unknown transport word, a
// malformed spec, or a fourth part — is reported unreadable so the caller can
// refuse instead of guessing where the media would go.
func parseRTSPTransport(spec string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(spec), "/")
	switch len(parts) {
	case 2:
		if !validTransportProfile(parts[0], parts[1]) {
			return "", false
		}
		return "udp", true
	case 3:
		if !validTransportProfile(parts[0], parts[1]) {
			return "", false
		}
		transport := strings.ToLower(parts[2])
		if transport != "udp" && transport != "tcp" {
			return "", false
		}
		return transport, true
	default:
		return "", false
	}
}

// validTransportProfile reports whether the protocol and profile words are the
// ones RFC 2326 defines. A different word means the rest of the header cannot
// be trusted to describe RTSP media.
func validTransportProfile(protocol, profile string) bool {
	return strings.EqualFold(protocol, "RTP") && strings.EqualFold(profile, "AVP")
}

// splitServerPorts reads a `server_port=a-b` value, defaulting the RTCP port to
// the RTP port plus one when the server gave a single port.
func splitServerPorts(value string) (int, int, bool) {
	first, second, hasSecond := strings.Cut(strings.TrimSpace(value), "-")
	rtpPort, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || rtpPort < 1 || rtpPort > 65535 {
		return 0, 0, false
	}
	rtcpPort := rtpPort + 1
	if rtcpPort > 65535 {
		return 0, 0, false
	}
	if hasSecond {
		parsed, err := strconv.Atoi(strings.TrimSpace(second))
		if err != nil || parsed < 1 || parsed > 65535 {
			return 0, 0, false
		}
		rtcpPort = parsed
	}
	return rtpPort, rtcpPort, true
}

// relayPortPair renders the two loopback relay ports in `a-b` form.
func relayPortPair(rtpAddr, rtcpAddr string) string {
	_, rtpPort, err := net.SplitHostPort(rtpAddr)
	if err != nil {
		return ""
	}
	_, rtcpPort, err := net.SplitHostPort(rtcpAddr)
	if err != nil {
		return ""
	}
	return rtpPort + "-" + rtcpPort
}

// guidanceInside reports whether the reply's source names the judged address.
// A host name is not re-resolved after the address decision, so only a literal
// equal to the judged address counts as inside (C-130, C-133).
func (r *rtspTransportRewriter) guidanceInside(source string) bool {
	host := strings.Trim(strings.TrimSpace(source), "[]")
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.Unmap() == r.judged.Unmap()
}

func (r *rtspTransportRewriter) recordBlocked(rtpPort, rtcpPort int, source string) {
	host := strings.Trim(strings.TrimSpace(source), "[]")
	r.media.ledger.recordBlocked(
		pushConnectionKind{family: "rtp", purpose: "derived"},
		net.JoinHostPort(host, strconv.Itoa(rtpPort)), pushOriginServer)
	r.media.ledger.recordBlocked(
		pushConnectionKind{family: "rtcp", purpose: "derived"},
		net.JoinHostPort(host, strconv.Itoa(rtcpPort)), pushOriginServer)
}
