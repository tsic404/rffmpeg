package worker

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
	"sync"
)

// TLS record content types (RFC 8446 §5.1).
const (
	tlsRecordAlert        = 21
	tlsRecordHandshake    = 22
	tlsHandshakeServerHel = 2
)

// proxyTunnelObserver answers the two questions C-140 and C-142 ask of a
// proxied push: was the receiver channel established, and did the TLS
// handshake the protocol needs complete? A TCP connection to the proxy is not
// connection evidence on its own, so the relay feeds both directions through
// this observer and reports its verdict instead of the dial result.
type proxyTunnelObserver struct {
	tls bool
	// headless marks a tunnel whose channel the worker itself established (the
	// judged proxy dialled the receiver and answered 200). There is no proxy
	// response to parse, so the channel counts as ready from the start.
	headless bool

	mu             sync.Mutex
	clientHeadDone bool
	serverHeadDone bool
	clientHead     []byte
	serverHead     []byte
	clientConnect  bool

	channelReady bool
	rejected     bool

	clientRecords  []byte
	serverRecords  []byte
	sawServerHello bool
	clientAdvanced bool
	tlsAlert       bool
}

func newProxyTunnelObserver(tls bool) *proxyTunnelObserver {
	return &proxyTunnelObserver{tls: tls}
}

// newEstablishedTunnelObserver watches a tunnel whose receiver channel the
// worker already opened, so only the TLS handshake is still unknown.
func newEstablishedTunnelObserver(tls bool) *proxyTunnelObserver {
	return &proxyTunnelObserver{tls: tls, headless: true, channelReady: true}
}

// ClientBytes consumes one chunk travelling from ffmpeg to the proxy.
func (o *proxyTunnelObserver) ClientBytes(chunk []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rejected {
		return
	}
	if o.headless {
		// The worker opened this channel itself, so the first client bytes are
		// the protocol's own (TLS records for an encrypted family), not a
		// proxy request.
		o.clientConnect = true
	} else if !o.clientHeadDone {
		head, rest, done := splitHTTPHead(o.clientHead, chunk)
		o.clientHead = head
		if !done {
			if len(o.clientHead) > pushHeaderBudget {
				o.rejected = true
			}
			return
		}
		o.clientHeadDone = true
		o.clientConnect = requestTargetsConnect(o.clientHead)
		chunk = rest
	}
	if !o.clientConnect || !o.tls || !o.channelReady {
		return
	}
	o.clientRecords = observeTLSRecords(o.clientRecords, chunk, func(typ byte, payload []byte) {
		switch {
		case typ == tlsRecordAlert:
			o.tlsAlert = true
		case o.sawServerHello:
			// The client's first handshake record is ClientHello. Anything it
			// sends after the server's ServerHello means the handshake carried
			// on: a TLS 1.2 Finished, a TLS 1.3 encrypted Finished, or the
			// media protocol riding the finished channel.
			o.clientAdvanced = true
		}
	})
}

// ServerBytes consumes one chunk travelling from the proxy to ffmpeg.
func (o *proxyTunnelObserver) ServerBytes(chunk []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rejected {
		return
	}
	if !o.headless && !o.serverHeadDone {
		head, rest, done := splitHTTPHead(o.serverHead, chunk)
		o.serverHead = head
		if !done {
			if len(o.serverHead) > pushHeaderBudget {
				o.rejected = true
			}
			return
		}
		o.serverHeadDone = true
		o.applyResponse(o.clientConnect, o.serverHead)
		chunk = rest
	}
	if !o.channelReady || !o.tls {
		return
	}
	o.serverRecords = observeTLSRecords(o.serverRecords, chunk, func(typ byte, payload []byte) {
		switch typ {
		case tlsRecordAlert:
			o.tlsAlert = true
		case tlsRecordHandshake:
			if len(payload) > 0 && payload[0] == tlsHandshakeServerHel {
				o.sawServerHello = true
			}
		}
	})
}

// applyResponse decides whether the proxy's answer established the channel. A
// CONNECT tunnel counts only on a 2xx; a plain proxied request counts once the
// proxy returns a response it did not generate itself, because 407 and the
// gateway errors are the proxy refusing or failing to reach the receiver.
func (o *proxyTunnelObserver) applyResponse(connect bool, head []byte) {
	code, ok := httpStatusCode(head)
	if !ok {
		o.rejected = true
		return
	}
	if connect {
		if code < 200 || code > 299 {
			o.rejected = true
			return
		}
		o.channelReady = true
		return
	}
	switch code {
	case 407, 502, 503, 504:
		o.rejected = true
	default:
		o.channelReady = true
	}
}

// Established is the C-140/C-142 verdict for this tunnel. Without TLS the
// receiver channel is the whole requirement. A TLS family additionally needs
// the receiver's ServerHello and the client carrying on past it, which is what
// proves the handshake completed rather than stalled or failed verification.
func (o *proxyTunnelObserver) Established() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.channelReady || o.rejected {
		return false
	}
	if !o.tls {
		return true
	}
	return o.sawServerHello && o.clientAdvanced && !o.tlsAlert
}

// Rejected reports that the proxy answered with a refusal or a gateway error,
// so the current address is unusable regardless of any later byte.
func (o *proxyTunnelObserver) Rejected() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.rejected
}

// splitHTTPHead appends chunk to buf and splits off the header that ends at the
// first blank line. rest is what followed the header on this chunk.
func splitHTTPHead(buf []byte, chunk []byte) (head, rest []byte, done bool) {
	buf = append(buf, chunk...)
	if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
		return buf[:i], buf[i+4:], true
	}
	return buf, nil, false
}

// requestTargetsConnect reports whether the request head asks the proxy for a
// CONNECT tunnel rather than naming an absolute URI.
func requestTargetsConnect(head []byte) bool {
	line := head
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(string(line))
	return len(fields) > 0 && strings.EqualFold(fields[0], "CONNECT")
}

// httpStatusCode reads the status code of a response head.
func httpStatusCode(head []byte) (int, bool) {
	line := head
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(string(line))
	if len(fields) < 2 || !strings.HasPrefix(strings.ToUpper(fields[0]), "HTTP/") {
		return 0, false
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil || code < 100 || code > 599 {
		return 0, false
	}
	return code, true
}

// observeTLSRecords walks the record layer of one direction, reporting every
// complete record and returning the bytes of a record still in flight.
func observeTLSRecords(buf []byte, chunk []byte, onRecord func(typ byte, payload []byte)) []byte {
	buf = append(buf, chunk...)
	for len(buf) >= 5 {
		length := int(binary.BigEndian.Uint16(buf[3:5]))
		if length > pushHeaderBudget {
			return nil
		}
		if len(buf) < 5+length {
			break
		}
		onRecord(buf[0], buf[5:5+length])
		buf = buf[len(buf)-len(buf[5+length:]):]
		buf = append([]byte(nil), buf...)
	}
	return buf
}

// pushSchemeUsesTLS reports whether a target scheme runs TLS on the connection
// itself, which is what makes the handshake part of C-142's `connected`.
func pushSchemeUsesTLS(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "https", "rtmps", "rtmpts", "rtsps":
		return true
	}
	return false
}
