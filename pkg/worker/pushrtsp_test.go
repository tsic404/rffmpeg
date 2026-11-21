package worker

import (
	"bytes"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

// judgedLoopbackForTest gives a loopback literal the relay can dial and a
// receiver can bind: every test here stays inside the test host.
func judgedLoopbackForTest(t *testing.T) (netip.Addr, bool) {
	t.Helper()
	return netip.MustParseAddr("127.0.0.1"), true
}

// sendDatagram writes one payload to a loopback relay and lets it forward.
func sendDatagram(t *testing.T, relayAddr, payload string) {
	t.Helper()
	conn, err := net.Dial("udp", relayAddr)
	if err != nil {
		t.Fatalf("dial relay %s: %v", relayAddr, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("send to relay %s: %v", relayAddr, err)
	}
}

func newTestMediaRelays(judged netip.Addr) *rtspMediaRelays {
	return &rtspMediaRelays{judged: judged, ledger: newPushLedger()}
}

// splitTransportPorts pulls the server_port= pair out of a rewritten Transport
// header so a test can compare ports without depending on ordering.
func splitTransportPorts(t *testing.T, header string) (string, string) {
	t.Helper()
	for _, part := range strings.Split(header, ";") {
		if strings.HasPrefix(part, "server_port=") {
			rtpPort, rtcpPort, ok := strings.Cut(strings.TrimPrefix(part, "server_port="), "-")
			if !ok {
				t.Fatalf("server_port is not a pair: %q", part)
			}
			return rtpPort, rtcpPort
		}
	}
	t.Fatalf("rewritten reply has no server_port: %q", header)
	return "", ""
}

func TestRTSPRewriterFrontsUDPServerPorts(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"), newTestMediaRelays(netip.MustParseAddr("10.77.0.2")))
	response := "RTSP/1.0 200 OK\r\nCSeq: 3\r\n" +
		"Transport: RTP/AVP;unicast;client_port=5000-5001;server_port=6000-6001;source=10.77.0.2\r\n\r\n"
	got := string(rewriter.Rewrite([]byte(response[:45]))) +
		string(rewriter.Rewrite([]byte(response[45:])))
	if strings.Contains(got, "server_port=6000-6001") {
		t.Fatalf("rewritten SETUP kept the server ports: %q", got)
	}
	if !strings.Contains(got, "source=127.0.0.1") {
		t.Fatalf("rewritten SETUP does not point ffmpeg at the relay: %q", got)
	}
	rtpPort, rtcpPort := splitTransportPorts(t, got)
	// The two relay ports belong to sockets that are bound and distinct.
	if rtpPort == rtcpPort {
		t.Fatalf("relay RTP and RTCP ports are equal: %q", got)
	}
	for _, raw := range []string{rtpPort, rtcpPort} {
		if _, err := strconv.Atoi(raw); err != nil {
			t.Fatalf("relay port %q is not a number", raw)
		}
	}
	// The pair really is a live datagram pair: sending on the RTP relay reaches
	// the judged address.
	flows := rewriter.media.flows()
	if len(flows) != 0 {
		t.Fatalf("flows without any send = %+v, want none", flows)
	}
	rewriter.media.close()
}

func TestRTSPRewriterRefusesGuidedSourceOutsideSet(t *testing.T) {
	judged := netip.MustParseAddr("10.77.0.2")
	media := newTestMediaRelays(judged)
	rewriter := newRTSPTransportRewriter(judged, media)
	response := "RTSP/1.0 200 OK\r\nCSeq: 7\r\n" +
		"Transport: RTP/AVP/UDP;unicast;server_port=6000-6001;source=10.99.0.9\r\n\r\n"
	got := string(rewriter.Rewrite([]byte(response)))
	if !strings.HasPrefix(got, "RTSP/1.0 461 ") {
		t.Fatalf("SETUP guided outside the judged set was honoured: %q", got)
	}
	blocked := media.ledger.snapshot()
	if len(blocked) != 2 {
		t.Fatalf("blocked elements = %+v, want one rtp and one rtcp", blocked)
	}
	for _, element := range blocked {
		if element.Result != "blocked" || element.Origin != "server" {
			t.Fatalf("element %+v is not a server-guided block", element)
		}
		if element.Target != "10.99.0.9:6000" && element.Target != "10.99.0.9:6001" {
			t.Fatalf("blocked element target = %q, want the guided address", element.Target)
		}
		if !pushKindAllowed(element.Family, element.Purpose) {
			t.Fatalf("family/purpose %s/%s is not a C-141 combination", element.Family, element.Purpose)
		}
	}
	if len(media.pairs) != 0 {
		t.Fatalf("a relay pair was opened for a blocked destination")
	}
}

func TestRTSPRewriterLeavesTCPInterleaved(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"), newTestMediaRelays(netip.MustParseAddr("10.77.0.2")))
	response := "RTSP/1.0 200 OK\r\nTransport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n\r\n"
	frame := []byte{0x24, 0x00, 0x00, 0x03, 'a', 'b', 'c'}
	got := append(rewriter.Rewrite([]byte(response)), rewriter.Rewrite(frame)...)
	if string(got) != response+string(frame) {
		t.Fatalf("interleaved reply was rewritten: %q", got)
	}
	if len(rewriter.media.pairs) != 0 {
		t.Fatal("interleaved media opened a datagram relay pair")
	}
}

func TestRTSPRewriterStreamsPartialInterleavedFrame(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"), newTestMediaRelays(netip.MustParseAddr("10.77.0.2")))
	header := []byte{0x24, 0x00, 0xff, 0xff}
	partial := append(append([]byte{}, header...), []byte("partial")...)
	got := rewriter.Rewrite(partial)
	if !bytes.Equal(got, partial) {
		t.Fatalf("partial interleaved frame was buffered: got %q, want %q", got, partial)
	}
}

func TestRTSPRewriterPassesBodyAndRewritesNextHeader(t *testing.T) {
	judged := netip.MustParseAddr("10.77.0.2")
	media := newTestMediaRelays(judged)
	rewriter := newRTSPTransportRewriter(judged, media)
	first := "RTSP/1.0 200 OK\r\nContent-Length: 5\r\nTransport: RTP/AVP;unicast;server_port=6000-6001;source=10.77.0.2\r\n\r\nhello"
	second := "RTSP/1.0 200 OK\r\nTransport: RTP/AVP;unicast;server_port=7000-7001;source=10.77.0.2\r\n\r\n"
	got := string(rewriter.Rewrite([]byte(first))) + string(rewriter.Rewrite([]byte(second)))
	if !strings.Contains(got, "\r\n\r\nhelloRTSP/1.0 200 OK") {
		t.Fatalf("body was not passed through: %q", got)
	}
	if strings.Count(got, "source=127.0.0.1") != 2 {
		t.Fatalf("both SETUP replies were not fronted: %q", got)
	}
	if len(media.pairs) != 2 {
		t.Fatalf("relay pairs = %d, want one per SETUP", len(media.pairs))
	}
	media.close()
}

func TestRTSPRewriterLeavesUnsupportedReply(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"), newTestMediaRelays(netip.MustParseAddr("10.77.0.2")))
	response := "RTSP/1.0 461 Unsupported Transport\r\nCSeq: 4\r\n\r\n"
	got := string(rewriter.Rewrite([]byte(response)))
	if got != response {
		t.Fatalf("unsupported reply was rewritten: %q", got)
	}
}

func TestRTSPRewriterFlushPreservesPartialMessage(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"), newTestMediaRelays(netip.MustParseAddr("10.77.0.2")))
	partial := []byte("RTSP/1.0 200 OK\r\nTransport: RTP/AVP;unicast;server_")
	if got := rewriter.Rewrite(partial); len(got) != 0 {
		t.Fatalf("partial header was forwarded early: %q", got)
	}
	if got := rewriter.Flush(); string(got) != string(partial) {
		t.Fatalf("Flush = %q, want the partial message unchanged", got)
	}
}

// TestRTSPMediaRelaysRecordDerivedFlows proves the pair reports an `rtp` and an
// `rtcp` element only after ffmpeg really sent on the relay, and that the
// element target is the judged address, not the loopback relay (C-134, C-141).
func TestRTSPMediaRelaysRecordDerivedFlows(t *testing.T) {
	addr, ok := judgedLoopbackForTest(t)
	if !ok {
		t.Skip("no usable loopback address for a datagram receiver")
	}
	rtpRecv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(addr.AsSlice())})
	if err != nil {
		t.Fatalf("listen for RTP: %v", err)
	}
	defer rtpRecv.Close()
	rtcpRecv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(addr.AsSlice())})
	if err != nil {
		t.Fatalf("listen for RTCP: %v", err)
	}
	defer rtcpRecv.Close()
	rtpPort := rtpRecv.LocalAddr().(*net.UDPAddr).Port
	rtcpPort := rtcpRecv.LocalAddr().(*net.UDPAddr).Port

	// A pair that never receives a datagram reports nothing: the list records
	// attempts, not negotiations.
	quiet := newTestMediaRelays(addr)
	if _, _, opened := quiet.open(rtpPort, rtcpPort); !opened {
		t.Fatal("could not open the quiet media relay pair")
	}
	if flows := quiet.flows(); len(flows) != 0 {
		t.Fatalf("flows without any send = %+v, want none", flows)
	}
	quiet.close()

	media := newTestMediaRelays(addr)
	defer media.close()
	rtpLocal, rtcpLocal, opened := media.open(rtpPort, rtcpPort)
	if !opened {
		t.Fatal("could not open the media relay pair")
	}
	sendDatagram(t, rtpLocal, "rtp-payload")
	sendDatagram(t, rtcpLocal, "rtcp-payload")

	flows := media.flows()
	if len(flows) != 2 {
		t.Fatalf("flows = %+v, want one rtp and one rtcp element", flows)
	}
	var sawRTP, sawRTCP bool
	for _, flow := range flows {
		if !pushKindAllowed(flow.Family, flow.Purpose) {
			t.Fatalf("family/purpose %s/%s is not a C-141 combination", flow.Family, flow.Purpose)
		}
		if flow.Purpose != "derived" {
			t.Fatalf("purpose = %q, want derived", flow.Purpose)
		}
		if flow.Result != "sent" {
			t.Fatalf("result = %q, want sent", flow.Result)
		}
		switch flow.Family {
		case "rtp":
			sawRTP = true
			if flow.Target != net.JoinHostPort(addr.String(), strconv.Itoa(rtpPort)) {
				t.Fatalf("rtp target = %q, want the judged address", flow.Target)
			}
		case "rtcp":
			sawRTCP = true
			if flow.Target != net.JoinHostPort(addr.String(), strconv.Itoa(rtcpPort)) {
				t.Fatalf("rtcp target = %q, want the judged address", flow.Target)
			}
		default:
			t.Fatalf("unexpected derived family %q", flow.Family)
		}
	}
	if !sawRTP || !sawRTCP {
		t.Fatalf("flows = %+v, want both rtp and rtcp", flows)
	}
}

// TestRTSPRewriterRefusesUnreadableTransport covers the reply the substring
// rule used to wave through: a first part that merely contains "/TCP" is not
// the TCP transport, and ffmpeg reads the media destination from ports it
// names. Any transport this code cannot read is refused, so no media is sent to
// a destination nobody pinned (C-130, C-137).
func TestRTSPRewriterRefusesUnreadableTransport(t *testing.T) {
	for _, spec := range []string{
		"RTP/AVP/TCPx;unicast;server_port=9876-9877;source=127.0.0.1",
		"RTP/AVP/SCTP;unicast;server_port=9876-9877;source=127.0.0.1",
		"RAW/AVP;unicast;server_port=9876-9877;source=127.0.0.1",
		"RTP/AVP/UDP/TCP;unicast;server_port=9876-9877;source=127.0.0.1",
	} {
		t.Run(spec, func(t *testing.T) {
			judged := netip.MustParseAddr("10.77.0.2")
			media := newTestMediaRelays(judged)
			rewriter := newRTSPTransportRewriter(judged, media)
			response := "RTSP/1.0 200 OK\r\nCSeq: 3\r\nTransport: " + spec + "\r\n\r\n"
			got := string(rewriter.Rewrite([]byte(response)))
			if !strings.Contains(got, "461 Unsupported Transport") {
				t.Fatalf("reply %q was not refused: %q", spec, got)
			}
			if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "9876") {
				t.Fatalf("a refused reply still names a media destination: %q", got)
			}
			if len(media.pairs) != 0 {
				t.Fatal("a refused reply opened a datagram relay pair")
			}
		})
	}
}

// TestRTSPRewriterStopsAtHeaderBudget proves a peer that never ends its header
// cannot grow the buffer without bound: the rewriter stops and the relay closes
// the connection instead of reading on (C-133).
func TestRTSPRewriterStopsAtHeaderBudget(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"), newTestMediaRelays(netip.MustParseAddr("10.77.0.2")))
	chunk := bytes.Repeat([]byte("x"), 32*1024)
	for i := 0; i < 8 && !rewriter.Failed(); i++ {
		if out := rewriter.Rewrite(chunk); len(out) != 0 {
			t.Fatalf("an unterminated header was forwarded: %d bytes", len(out))
		}
	}
	if !rewriter.Failed() {
		t.Fatal("the rewriter kept buffering a header past its budget")
	}
	if len(rewriter.buf) != 0 {
		t.Fatalf("the rewriter kept %d buffered bytes after giving up", len(rewriter.buf))
	}
}

// TestParseRTSPTransport locks the transport spec reading to the three forms
// RFC 2326 defines for this protocol family; a two-part spec means UDP.
func TestParseRTSPTransport(t *testing.T) {
	for _, tc := range []struct {
		spec      string
		transport string
		ok        bool
	}{
		{"RTP/AVP", "udp", true},
		{"RTP/AVP/UDP", "udp", true},
		{"RTP/AVP/TCP", "tcp", true},
		{"rtp/avp/tcp", "tcp", true},
		{"RTP/AVP/TCPx", "", false},
		{"RTP/AVP/SCTP", "", false},
		{"TCP", "", false},
		{"RTP/AVP/UDP/TCP", "", false},
		{"", "", false},
	} {
		transport, ok := parseRTSPTransport(tc.spec)
		if ok != tc.ok || transport != tc.transport {
			t.Errorf("parseRTSPTransport(%q) = (%q, %v), want (%q, %v)", tc.spec, transport, ok, tc.transport, tc.ok)
		}
	}
}
