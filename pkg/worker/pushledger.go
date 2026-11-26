package worker

import (
	"sync"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// The `result` values of C-142.
const (
	pushResultConnected   = "connected"
	pushResultUnreachable = "unreachable"
	pushResultSent        = "sent"
	pushResultFailed      = "failed"
	pushResultBlocked     = "blocked"
)

// The `origin` values of C-134.
const (
	pushOriginRequest = "request"
	pushOriginServer  = "server"
)

// pushConnectionKind is one `family` / `purpose` combination. C-141 fixes the
// complete set; nothing outside these tables may reach a connection list.
type pushConnectionKind struct {
	family  string
	purpose string
}

// pushSessionFamilies maps a push scheme to the `family` of its session
// connection. The table is transcribed from C-141 and is the only source of
// legal session families: a scheme absent here is outside the contract and
// must fail as INFRA before any connection is made (C-138).
var pushSessionFamilies = map[string]string{
	"rtmp":  "rtmp",
	"rtmps": "rtmps",
	"rtmpt": "rtmpt",
	"http":  "http",
	"https": "https",
	"rtsp":  "rtsp",
	"rtsps": "rtsps",
	"srt":   "srt",
	"ftp":   "ftp",
}

// pushStreamFamilies is the C-141 `stream` half: single-datagram outputs.
// rtp_mpegts is ffmpeg's second spelling of the standalone RTP muxer, so it
// reports as `rtp`.
var pushStreamFamilies = map[string]string{
	"udp":        "udp",
	"rtp":        "rtp",
	"rtp_mpegts": "rtp",
}

// pushSessionKind returns the C-141 session combination for scheme.
func pushSessionKind(scheme string) (pushConnectionKind, bool) {
	family, ok := pushSessionFamilies[scheme]
	if !ok {
		return pushConnectionKind{}, false
	}
	return pushConnectionKind{family: family, purpose: "session"}, true
}

// pushStreamKind returns the C-141 stream combination for scheme.
func pushStreamKind(scheme string) (pushConnectionKind, bool) {
	family, ok := pushStreamFamilies[scheme]
	if !ok {
		return pushConnectionKind{}, false
	}
	return pushConnectionKind{family: family, purpose: "stream"}, true
}

// pushKindAllowed reports whether a combination is one C-141 defines. Tests and
// reviewers use it to check a produced list without re-reading the table.
func pushKindAllowed(family, purpose string) bool {
	switch purpose {
	case "session":
		for _, f := range pushSessionFamilies {
			if f == family {
				return true
			}
		}
	case "stream":
		for _, f := range pushStreamFamilies {
			if f == family {
				return true
			}
		}
	case "derived":
		return family == "rtp" || family == "rtcp" || family == "fec" || family == "ftp"
	}
	return false
}

// pushDatagramFlow is one datagram relay's report of what it attempted. It is
// filled by the relay itself, so the element reflects a real send attempt
// rather than a guess made from ffmpeg's exit status (C-135).
type pushDatagramFlow struct {
	Family  string
	Purpose string
	Target  string
	Result  string
	// Origin is `request` when the destination came from the request or its
	// parameters, and `server` when a protocol reply guided it (C-134). An
	// empty value means `request`, which is the case for every flow the
	// request itself specified.
	Origin string
}

// pushLedger accumulates the connection list of one job. Elements are appended
// as attempts finish, so the list a terminal report carries is the record of
// what the worker actually did.
type pushLedger struct {
	mu       sync.Mutex
	elements []protocol.PushConnection
}

func newPushLedger() *pushLedger {
	return &pushLedger{}
}

func (l *pushLedger) add(element protocol.PushConnection) {
	l.mu.Lock()
	l.elements = append(l.elements, element)
	l.mu.Unlock()
}

// recordSession appends the session element of one judged address. A relay that
// never accepted a connection from ffmpeg attempted nothing and contributes no
// element.
func (l *pushLedger) recordSession(kind pushConnectionKind, target string, attempted, connected bool) {
	if !attempted {
		return
	}
	result := pushResultUnreachable
	if connected {
		result = pushResultConnected
	}
	l.add(protocol.PushConnection{
		Family:  kind.family,
		Purpose: kind.purpose,
		Target:  target,
		Origin:  pushOriginRequest,
		Result:  result,
	})
}

// recordSessionOrigin appends a session element whose target came from the
// request parameters rather than from the target itself — a request-named proxy
// (C-140). The origin is still `request`, because the request chose it.
func (l *pushLedger) recordSessionOrigin(kind pushConnectionKind, target string, attempted, connected bool, origin string) {
	if !attempted {
		return
	}
	result := pushResultUnreachable
	if connected {
		result = pushResultConnected
	}
	l.add(protocol.PushConnection{
		Family:  kind.family,
		Purpose: kind.purpose,
		Target:  target,
		Origin:  origin,
		Result:  result,
	})
}

// recordDatagrams appends one element per datagram flow that was attempted.
func (l *pushLedger) recordDatagrams(flows []pushDatagramFlow) {
	for _, flow := range flows {
		origin := flow.Origin
		if origin == "" {
			origin = pushOriginRequest
		}
		l.add(protocol.PushConnection{
			Family:  flow.Family,
			Purpose: flow.Purpose,
			Target:  flow.Target,
			Origin:  origin,
			Result:  flow.Result,
		})
	}
}

// recordBlocked appends the element of a target that lies outside the judged
// address set, so no connection was attempted (C-130, C-140, C-142). The
// element keeps the destination the request or the server named, because that
// is the address the record is about.
func (l *pushLedger) recordBlocked(kind pushConnectionKind, target, origin string) {
	l.add(protocol.PushConnection{
		Family:  kind.family,
		Purpose: kind.purpose,
		Target:  target,
		Origin:  origin,
		Result:  pushResultBlocked,
	})
}

// snapshot returns the list recorded so far. The caller gets its own copy: the
// report and any later inspection must not share the ledger's slice.
func (l *pushLedger) snapshot() []protocol.PushConnection {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.elements) == 0 {
		return nil
	}
	out := make([]protocol.PushConnection, len(l.elements))
	copy(out, l.elements)
	return out
}

// mark records the list length, so one attempt's contribution can be judged on
// its own. Address switching and the per-address usability decision read these
// elements, never a parallel pair of relay counters (C-135).
func (l *pushLedger) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.elements)
}

// attemptedSince reports whether the attempt started at mark recorded any
// connection or datagram flow.
func (l *pushLedger) attemptedSince(mark int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.elements) > mark
}

// usableSince reports whether the attempt started at mark made this address
// usable: a session connected or a datagram flow was sent, and no element of
// the attempt needs a destination outside the judged set (C-137). A `blocked`
// element is exactly that need, so it overrides a successful connection.
func (l *pushLedger) usableSince(mark int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	usable := false
	for _, element := range l.elements[mark:] {
		switch element.Result {
		case pushResultBlocked:
			return false
		case pushResultConnected, pushResultSent:
			usable = true
		}
	}
	return usable
}

// attempted reports whether any connection or datagram flow was made at all.
func (l *pushLedger) attempted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.elements) > 0
}

// usable reports whether some judged address turned out usable: a session
// connection established or a datagram flow was sent. C-137's "collection
// entirely unusable" is the negation of this, judged from the list alone.
func (l *pushLedger) usable() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, element := range l.elements {
		if element.Result == pushResultConnected || element.Result == pushResultSent {
			return true
		}
	}
	return false
}

// rememberPushLedger stores the ledger a job's terminal report will read.
func (w *Worker) rememberPushLedger(jobID string, ledger *pushLedger) {
	w.pushLedgers.Store(jobID, ledger)
}

// takePushList detaches the job's connection list for its terminal report. The
// ledger is dropped on read: a job reaches a terminal state once, and a stale
// list must not leak into a later report.
func (w *Worker) takePushList(jobID string) []protocol.PushConnection {
	value, ok := w.pushLedgers.LoadAndDelete(jobID)
	if !ok {
		return nil
	}
	ledger, ok := value.(*pushLedger)
	if !ok {
		return nil
	}
	return ledger.snapshot()
}
