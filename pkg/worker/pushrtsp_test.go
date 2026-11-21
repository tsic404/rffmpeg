package worker

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
)

func TestRTSPRewriterPinsUDPSource(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"))
	response := "RTSP/1.0 200 OK\r\nCSeq: 3\r\n" +
		"Transport: RTP/AVP;unicast;client_port=5000-5001;server_port=6000-6001;source=127.0.0.1\r\n\r\n"
	got := string(rewriter.Rewrite([]byte(response[:45]))) +
		string(rewriter.Rewrite([]byte(response[45:])))
	if strings.Contains(got, "127.0.0.1") {
		t.Fatalf("rewritten SETUP kept the server source: %q", got)
	}
	if !strings.Contains(got, "source=10.77.0.2") {
		t.Fatalf("rewritten SETUP has no judged source: %q", got)
	}
	if !strings.Contains(got, "server_port=6000-6001") {
		t.Fatalf("rewritten SETUP lost the negotiated ports: %q", got)
	}
}

func TestRTSPRewriterAddsSourceWhenAbsent(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"))
	response := "RTSP/1.0 200 OK\r\nTransport: RTP/AVP/UDP;unicast;server_port=6000-6001\r\n\r\n"
	got := string(rewriter.Rewrite([]byte(response)))
	if !strings.Contains(got, "source=10.77.0.2") {
		t.Fatalf("rewritten SETUP has no judged source: %q", got)
	}
}

func TestRTSPRewriterLeavesTCPInterleaved(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"))
	response := "RTSP/1.0 200 OK\r\nTransport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n\r\n"
	frame := []byte{0x24, 0x00, 0x00, 0x03, 'a', 'b', 'c'}
	got := append(rewriter.Rewrite([]byte(response)), rewriter.Rewrite(frame)...)
	if string(got) != response+string(frame) {
		t.Fatalf("interleaved reply was rewritten: %q", got)
	}
}

func TestRTSPRewriterStreamsPartialInterleavedFrame(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"))
	header := []byte{0x24, 0x00, 0xff, 0xff}
	partial := append(append([]byte{}, header...), []byte("partial")...)
	got := rewriter.Rewrite(partial)
	if !bytes.Equal(got, partial) {
		t.Fatalf("partial interleaved frame was buffered: got %q, want %q", got, partial)
	}
}

func TestRTSPRewriterPassesBodyAndRewritesNextHeader(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"))
	first := "RTSP/1.0 200 OK\r\nContent-Length: 5\r\nTransport: RTP/AVP;unicast;server_port=6000-6001\r\n\r\nhello"
	second := "RTSP/1.0 200 OK\r\nTransport: RTP/AVP;unicast;server_port=7000-7001\r\n\r\n"
	got := string(rewriter.Rewrite([]byte(first))) + string(rewriter.Rewrite([]byte(second)))
	if !strings.Contains(got, "\r\n\r\nhelloRTSP/1.0 200 OK") {
		t.Fatalf("body was not passed through: %q", got)
	}
	if strings.Count(got, "source=10.77.0.2") != 2 {
		t.Fatalf("both SETUP replies were not pinned: %q", got)
	}
}

func TestRTSPRewriterLeavesUnsupportedReply(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"))
	response := "RTSP/1.0 461 Unsupported Transport\r\nCSeq: 4\r\n\r\n"
	got := string(rewriter.Rewrite([]byte(response)))
	if got != response {
		t.Fatalf("unsupported reply was rewritten: %q", got)
	}
}

func TestRTSPRewriterFlushPreservesPartialMessage(t *testing.T) {
	rewriter := newRTSPTransportRewriter(netip.MustParseAddr("10.77.0.2"))
	partial := []byte("RTSP/1.0 200 OK\r\nTransport: RTP/AVP;unicast;server_")
	if got := rewriter.Rewrite(partial); len(got) != 0 {
		t.Fatalf("partial header was forwarded early: %q", got)
	}
	if got := rewriter.Flush(); string(got) != string(partial) {
		t.Fatalf("Flush = %q, want the partial message unchanged", got)
	}
}
