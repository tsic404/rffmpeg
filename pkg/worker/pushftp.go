package worker

import (
	"bytes"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
)

// ftpDataRelays owns the data connections one FTP session opens. The control
// relay's rewriter starts a relay for every passive reply whose guidance stays
// inside the judged set, and records one `ftp`/`derived` element per data
// connection the relay actually saw (C-134, C-141).
type ftpDataRelays struct {
	judged netip.Addr
	ledger *pushLedger

	mu     sync.Mutex
	relays []*ftpDataRelay
}

type ftpDataRelay struct {
	relay  *judgedTCPRelay
	target string
}

// open starts a relay for one data connection and returns the loopback address
// the reply must name.
func (d *ftpDataRelays) open(port int) (string, bool) {
	target := net.JoinHostPort(d.judged.String(), strconv.Itoa(port))
	relay, err := startJudgedTCPRelay(target)
	if err != nil {
		return "", false
	}
	d.mu.Lock()
	d.relays = append(d.relays, &ftpDataRelay{relay: relay, target: target})
	d.mu.Unlock()
	return relay.Addr(), true
}

// collect records one element per data connection and releases the listeners.
// A data relay ffmpeg never connected to attempted nothing and adds no element,
// the same rule the control session follows.
func (d *ftpDataRelays) collect() {
	d.mu.Lock()
	relays := d.relays
	d.relays = nil
	d.mu.Unlock()
	for _, data := range relays {
		attempted, established := data.relay.Observed()
		// The data destination comes from the server's passive reply, so the
		// element's origin is the server's guidance even though the socket
		// itself dials the judged address (C-130, C-134).
		d.ledger.recordSessionOrigin(pushConnectionKind{family: "ftp", purpose: "derived"}, data.target, attempted, established, pushOriginServer)
		data.relay.Close()
	}
}

func (d *ftpDataRelays) close() {
	d.mu.Lock()
	relays := d.relays
	d.relays = nil
	d.mu.Unlock()
	for _, data := range relays {
		data.relay.Close()
	}
}

// ftpReplyRewriter rewrites the passive-mode replies of the FTP control
// connection. ffmpeg takes the data destination from those replies, so an
// inside-set guidance is repointed at a local relay that dials the judged
// address, while an outside-set one never gets a connection at all (C-130).
type ftpReplyRewriter struct {
	judged netip.Addr
	data   *ftpDataRelays
	ledger *pushLedger
	buf    []byte
}

func newFTPReplyRewriter(judged netip.Addr, data *ftpDataRelays, ledger *pushLedger) *ftpReplyRewriter {
	return &ftpReplyRewriter{judged: judged, data: data, ledger: ledger}
}

// Rewrite buffers the control stream until complete reply lines are available,
// then rewrites the passive-mode ones. Every other byte is forwarded unchanged.
func (r *ftpReplyRewriter) Rewrite(chunk []byte) []byte {
	r.buf = append(r.buf, chunk...)
	var out []byte
	for {
		end := bytes.Index(r.buf, []byte("\r\n"))
		if end < 0 {
			break
		}
		out = append(out, r.rewriteLine(string(r.buf[:end]))...)
		out = append(out, '\r', '\n')
		r.buf = r.buf[end+2:]
	}
	return out
}

// Flush returns a trailing partial line unchanged rather than swallowing it.
func (r *ftpReplyRewriter) Flush() []byte {
	out := append([]byte{}, r.buf...)
	r.buf = nil
	return out
}

// Failed always reports false: a passive-mode reply is one short line, so this
// rewriter has no budget to exceed.
func (r *ftpReplyRewriter) Failed() bool { return false }

func (r *ftpReplyRewriter) rewriteLine(line string) []byte {
	switch {
	case strings.HasPrefix(line, "227 "):
		return []byte(r.rewritePASV(line))
	case strings.HasPrefix(line, "229 "):
		return []byte(r.rewriteEPSV(line))
	default:
		return []byte(line)
	}
}

// rewritePASV handles `227 ... (h1,h2,h3,h4,p1,p2)`. The announced address is
// the server's guidance. The judged address — or the all-zero wildcard, which
// means "the control connection's host" — keeps the data connection on this
// attempt; any other address is recorded as blocked and answered with a
// refusal, so the data connection is never attempted (C-130).
func (r *ftpReplyRewriter) rewritePASV(line string) string {
	open := strings.LastIndex(line, "(")
	closeIdx := strings.LastIndex(line, ")")
	if open < 0 || closeIdx < open {
		return line
	}
	fields := strings.Split(line[open+1:closeIdx], ",")
	if len(fields) != 6 {
		return line
	}
	var octets [4]byte
	for i := 0; i < 4; i++ {
		n, err := strconv.Atoi(strings.TrimSpace(fields[i]))
		if err != nil || n < 0 || n > 255 {
			return line
		}
		octets[i] = byte(n)
	}
	high, err1 := strconv.Atoi(strings.TrimSpace(fields[4]))
	low, err2 := strconv.Atoi(strings.TrimSpace(fields[5]))
	if err1 != nil || err2 != nil || high < 0 || high > 255 || low < 0 || low > 255 {
		return line
	}
	announced := netip.AddrFrom4(octets)
	port := high*256 + low
	if !r.guidanceInside(announced) {
		r.ledger.recordBlocked(
			pushConnectionKind{family: "ftp", purpose: "derived"},
			net.JoinHostPort(announced.String(), strconv.Itoa(port)),
			pushOriginServer)
		return "425 Cannot open data connection"
	}
	local, ok := r.data.open(port)
	if !ok {
		return line
	}
	return fmtPASV(line, open, closeIdx, local)
}

// rewriteEPSV handles `229 ... (|||port|)`. Extended passive mode carries no
// address, so its guidance is always the control connection's judged address.
func (r *ftpReplyRewriter) rewriteEPSV(line string) string {
	open := strings.LastIndex(line, "(")
	closeIdx := strings.LastIndex(line, ")")
	if open < 0 || closeIdx < open {
		return line
	}
	parts := strings.Split(line[open+1:closeIdx], "|")
	if len(parts) < 4 {
		return line
	}
	port, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-2]))
	if err != nil || port < 1 || port > 65535 {
		return line
	}
	local, ok := r.data.open(port)
	if !ok {
		return line
	}
	_, localPort, err := net.SplitHostPort(local)
	if err != nil {
		return line
	}
	return line[:open+1] + "|||" + localPort + "|" + line[closeIdx:]
}

// guidanceInside reports whether a server-guided address is the judged one. The
// all-zero address is a wildcard meaning "the control connection's host", which
// is the judged address (C-130).
func (r *ftpReplyRewriter) guidanceInside(addr netip.Addr) bool {
	if addr.IsUnspecified() {
		return true
	}
	return addr.Unmap() == r.judged.Unmap()
}

// fmtPASV rewrites a `227` reply's address fields to point at a loopback relay.
func fmtPASV(line string, open, closeIdx int, local string) string {
	localHost, localPort, err := net.SplitHostPort(local)
	if err != nil {
		return line
	}
	addr, err := netip.ParseAddr(localHost)
	if err != nil {
		return line
	}
	port, err := strconv.Atoi(localPort)
	if err != nil || port < 0 || port > 65535 {
		return line
	}
	b := addr.As4()
	fields := []string{
		strconv.Itoa(int(b[0])), strconv.Itoa(int(b[1])), strconv.Itoa(int(b[2])), strconv.Itoa(int(b[3])),
		strconv.Itoa(port / 256), strconv.Itoa(port % 256),
	}
	return line[:open+1] + strings.Join(fields, ",") + line[closeIdx:]
}
