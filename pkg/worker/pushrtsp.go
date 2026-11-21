package worker

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"strconv"
	"strings"
)

// rtspTransportRewriter rewrites SETUP replies on the upstream side of the
// RTSP control relay. The relay dials the judged address, so ffmpeg's TCP peer
// is loopback; without a source parameter ffmpeg would send RTP/RTCP to that
// loopback peer. Replacing source with the judged IP literal keeps every
// negotiated UDP datagram on the address the relay actually reached, even when
// the server names a different source.
type rtspTransportRewriter struct {
	judged      netip.Addr
	buf         []byte
	body        int
	interleaved int
}

func newRTSPTransportRewriter(judged netip.Addr) *rtspTransportRewriter {
	return &rtspTransportRewriter{judged: judged}
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
	for i, line := range lines {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Transport") {
			continue
		}
		lines[i] = "Transport: " + r.rewriteTransport(strings.TrimSpace(value))
	}
	return []byte(strings.Join(lines, "\r\n")), contentLength(lines)
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

func (r *rtspTransportRewriter) rewriteTransport(value string) string {
	parts := strings.Split(value, ";")
	trimmed := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			trimmed = append(trimmed, part)
		}
	}
	if len(trimmed) == 0 {
		return value
	}
	// Interleaved TCP already carries media on the judged control connection,
	// and multicast has no unicast source to pin. Leave both untouched.
	if strings.Contains(strings.ToUpper(trimmed[0]), "/TCP") {
		return value
	}
	hasServerPort := false
	for _, part := range trimmed {
		if strings.EqualFold(part, "multicast") {
			return value
		}
		if strings.HasPrefix(strings.ToLower(part), "server_port=") {
			hasServerPort = true
		}
	}
	if !hasServerPort {
		return value
	}
	kept := make([]string, 0, len(trimmed)+1)
	for _, part := range trimmed {
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, "source=") || strings.HasPrefix(lower, "destination=") {
			continue
		}
		kept = append(kept, part)
	}
	kept = append(kept, "source="+r.judged.String())
	return strings.Join(kept, ";")
}
