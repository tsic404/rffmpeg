package worker

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/addrpolicy"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// boundaryFailure is an execution-side trust-boundary violation: the job fails
// with INFRA, ffmpeg never starts, and nothing outside the workspace is
// created, overwritten or removed (C-124 / C-126 / C-127).
type boundaryFailure struct {
	msg     string
	details string
}

// pathSpace returns the declared shared filesystem space, never nil so callers
// can ask for membership unconditionally. An undeclared space has no legal
// path (ADR-018 option A).
func (w *Worker) pathSpace() *pathutil.Space {
	if w.sharedFS == nil {
		return pathutil.NewSpace("")
	}
	return w.sharedFS
}

// jobInput is one planned input of a job: its classification form plus the
// identifier itself.
type jobInput struct {
	form       pathutil.InputForm
	identifier string
}

// planJobInputs classifies every input identifier of a job (C-123 / C-124).
// Classification is the only basis on which the execution side obtains inputs,
// so it runs before the job workspace exists: an identifier that matches no
// form must not leave a single file behind. A direct-mode job takes its inputs
// from the pass-through field (C-125), and each of those must classify as a
// shared filesystem path as well.
func (w *Worker) planJobInputs(job protocol.JobInfo, directMode bool) ([]jobInput, *boundaryFailure) {
	space := w.pathSpace()
	planned := make([]jobInput, 0, len(job.InputFiles))
	for _, identifier := range job.InputFiles {
		form, err := pathutil.ClassifyInput(identifier, space)
		if err != nil {
			return nil, &boundaryFailure{
				msg:     "input identifier cannot be classified: " + identifier,
				details: err.Error(),
			}
		}
		if directMode && form != pathutil.InputSharedPath {
			return nil, &boundaryFailure{
				msg: fmt.Sprintf("a pass-through job accepts only shared filesystem paths as inputs: %q is a %s",
					identifier, form),
				details: "input form does not match the pass-through mode",
			}
		}
		planned = append(planned, jobInput{form: form, identifier: identifier})
	}

	if directMode {
		// C-125: the pass-through field is the input list of a direct-mode job.
		planned = planned[:0]
		for _, directPath := range job.DirectPaths {
			if err := pathutil.ValidateSharedPath(space, directPath); err != nil {
				return nil, &boundaryFailure{
					msg:     "pass-through input is outside the declared shared filesystem space",
					details: err.Error(),
				}
			}
			planned = append(planned, jobInput{form: pathutil.InputSharedPath, identifier: directPath})
		}
	}
	return planned, nil
}

// jobOutput is the judged output target of a job: the class it belongs to and
// the value ffmpeg receives. A protocol address target carries the judged
// address set as well, so the connection can be pinned to those addresses
// instead of a fresh resolution inside ffmpeg (C-129); every other target is a
// local path (C-125).
type jobOutput struct {
	target    string
	form      pathutil.OutputTargetForm
	addresses []netip.Addr
}

// requestHost is the protocol name the request gave the target. Protocols
// that can carry the name separately from the connection keep this value
// while the socket goes to a judged address.
func (o jobOutput) requestHost() string {
	parsed, err := url.Parse(o.target)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// targetAt returns the target ffmpeg receives for one judged address. The
// request's host name is replaced by the judged IP because the protocols this
// deployment pushes with carry no separate connect-address field; C-129
// registers that as the known limitation. The port, user info, path and query
// are preserved.
func (o jobOutput) targetAt(addr netip.Addr) (string, error) {
	parsed, err := url.Parse(o.target)
	if err != nil {
		return "", fmt.Errorf("invalid output target %q: %w", o.target, err)
	}
	host := addr.String()
	if addr.Is6() {
		host = "[" + host + "]"
	}
	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(addr.String(), port)
	}
	parsed.Host = host
	return parsed.String(), nil
}

// targetAtHost returns the target ffmpeg receives when a worker-owned relay
// fronts the judged address. The relay address replaces the request host; the
// port, user info, path and query are preserved.
func (o jobOutput) targetAtHost(hostport string) (string, error) {
	parsed, err := url.Parse(o.target)
	if err != nil {
		return "", fmt.Errorf("invalid output target %q: %w", o.target, err)
	}
	parsed.Host = hostport
	return parsed.String(), nil
}

// pushTransport reports how this target's connection is kept on the judged set.
func (o jobOutput) pushTransport() pushTransport {
	return pushTransportFor(o.target)
}

// judgedHostPort is the judged endpoint the relay dials. A target without an
// explicit port uses the protocol default; a protocol with no known default
// cannot be relayed and fails closed rather than guessing.
func (o jobOutput) judgedHostPort(addr netip.Addr) (string, error) {
	parsed, err := url.Parse(o.target)
	if err != nil {
		return "", fmt.Errorf("invalid output target %q: %w", o.target, err)
	}
	port := parsed.Port()
	if port == "" {
		var ok bool
		port, ok = defaultPushPort(strings.ToLower(parsed.Scheme))
		if !ok {
			return "", fmt.Errorf("output target %q carries no port", o.target)
		}
	}
	return net.JoinHostPort(addr.String(), port), nil
}

// defaultPushPort is the port ffmpeg itself would use for a scheme whose URL
// omits one.
func defaultPushPort(scheme string) (string, bool) {
	switch scheme {
	case "http":
		return "80", true
	case "https":
		return "443", true
	case "rtmp":
		return "1935", true
	case "rtmps":
		return "443", true
	case "rtmpt":
		return "80", true
	case "rtmpts":
		return "443", true
	case "rtsp":
		return "554", true
	case "rtsps":
		return "322", true
	case "srt":
		return "9000", true
	default:
		return "", false
	}
}

// srtHandshake reports whether the datagram protocol has a caller handshake,
// which makes an upstream response the connection-established fact.
func (o jobOutput) srtHandshake() bool {
	return pushTargetScheme(o.target) == "srt"
}

// ffmpegOutputOptions returns the protocol options that keep a push session
// within the judged connection and preserve the requested host name where the
// protocol can carry it separately.
func (o jobOutput) ffmpegOutputOptions(existingArgs []string) []string {
	parsed, err := url.Parse(o.target)
	if err != nil {
		return nil
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "http" || scheme == "https" {
		// A redirect is a second connection to a URL ffmpeg chooses, which can
		// be outside the judged set. Refusing redirects is the only way the
		// HTTP protocol keeps every connection on the judged address. The
		// relay takes the socket, so the request host travels in the Host
		// header instead. Request-supplied headers are kept in full and only
		// extended with that Host unless the request already chose one.
		options := []string{"-max_redirects", "0"}
		hostHeader := "Host: " + parsed.Host + "\r\n"
		if existing := ffmpegOptionValue(outputSideFFmpegArgs(existingArgs), "-headers"); existing != "" {
			if headerHasHost(existing) {
				return options
			}
			if !strings.HasSuffix(existing, "\n") {
				existing += "\r\n"
			}
			hostHeader = existing + hostHeader
		}
		return append(options, "-headers", hostHeader)
	}
	if scheme == "rtsps" {
		// Keep whatever transport the request asked for. ffmpeg itself pins
		// rtsps to interleaved TCP (libavformat/rtsp.c sets the lower transport
		// mask to TCP for the rtsps protocol, whatever the option says), so the
		// media already rides the judged control connection and the worker has
		// nothing to override (C-133, C-137).
		return nil
	}
	if strings.HasPrefix(scheme, "rtmp") {
		outputArgs := outputSideFFmpegArgs(existingArgs)
		if hasFFmpegOption(outputArgs, "-rtmp_tcurl") {
			return nil
		}
		app := ffmpegOptionValue(outputArgs, "-rtmp_app")
		if app == "" {
			requestPath := parsed.EscapedPath()
			if parsed.RawQuery != "" {
				requestPath += "?" + parsed.RawQuery
			}
			app = rtmpAppFromPath(requestPath)
		}
		return []string{"-rtmp_tcurl", scheme + "://" + parsed.Host + "/" + app}
	}
	return nil
}

// pushProxyEnvNames are the environment variables ffmpeg consults to decide
// whether to send a connection through a proxy. A push may only use a proxy the
// request named (C-140), so a push execution starts with these cleared: a
// worker deployed behind a proxy would otherwise let ffmpeg open a connection
// outside the judged set instead of using the relay.
var pushProxyEnvNames = []string{"http_proxy", "https_proxy", "all_proxy", "no_proxy"}

// clearedPushProxyEnv returns the variables that neutralise an inherited proxy
// configuration. Both spellings are set because libc reads the name as given
// and ffmpeg accepts either.
func clearedPushProxyEnv() []string {
	env := make([]string, 0, len(pushProxyEnvNames)*2)
	for _, name := range pushProxyEnvNames {
		env = append(env, name+"=", strings.ToUpper(name)+"=")
	}
	return env
}

// requestProxy describes a proxy the request itself named (C-140). HTTP and
// RTMPT are the protocols whose request parameters can carry one; for every
// other protocol the option has no effect and is not a proxy.
type requestProxy struct {
	inside bool
	target string
}

// requestProxyFor judges the request's `-http_proxy` against this attempt's
// judged address. A host name is not re-resolved: only the request's own name
// maps to the current address, and anything else is outside the set (C-133).
func (o jobOutput) requestProxyFor(existingArgs []string, addr netip.Addr) (requestProxy, bool) {
	scheme := pushTargetScheme(o.target)
	if scheme != "http" && scheme != "https" && !strings.HasPrefix(scheme, "rtmpt") {
		return requestProxy{}, false
	}
	value := ffmpegOptionValue(outputSideFFmpegArgs(existingArgs), "-http_proxy")
	if value == "" {
		return requestProxy{}, false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return requestProxy{}, false
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		host, port = strings.Trim(parsed.Host, "[]"), "80"
	}
	judged := false
	if literal, err := netip.ParseAddr(host); err == nil {
		judged = literal.Unmap() == addr.Unmap()
	} else {
		judged = strings.EqualFold(host, o.requestHostname())
	}
	target := net.JoinHostPort(addr.String(), port)
	if !judged {
		target = net.JoinHostPort(host, port)
	}
	return requestProxy{inside: judged, target: target}, true
}

// replaceFFmpegOptionValue returns args with the last value of name replaced.
// It is how a request-named proxy is repointed at its local relay without
// touching any other request option.
func replaceFFmpegOptionValue(args []string, name, value string) []string {
	out := append([]string{}, args...)
	for i := 0; i < len(out); i++ {
		if out[i] == name && i+1 < len(out) {
			out[i+1] = value
			continue
		}
		if strings.HasPrefix(out[i], name+"=") {
			out[i] = name + "=" + value
		}
	}
	return out
}

// requestHostname is the request's target host without its port.
func (o jobOutput) requestHostname() string {
	host, _, err := net.SplitHostPort(o.requestHost())
	if err != nil {
		return strings.Trim(o.requestHost(), "[]")
	}
	return host
}

// pushKindError refuses a push whose protocol family — or a connection kind the
// request asks for — is not in the C-141 table. It runs before any connection is
// made, so the job fails as INFRA with an empty connection list (C-133, C-138).
func (o jobOutput) pushKindError(existingArgs []string) error {
	scheme := pushTargetScheme(o.target)
	if _, ok := pushSessionKind(scheme); ok {
		if scheme != "rtsp" && scheme != "rtsps" {
			return nil
		}
		// RTSP's transport selects the media connection kind. Only TCP
		// (interleaved media on the control connection) and UDP (derived
		// RTP/RTCP) are listed; anything else has no row to record.
		value := ffmpegOptionValue(outputSideFFmpegArgs(existingArgs), "-rtsp_transport")
		if value == "" {
			return nil
		}
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '+' || r == ',' }) {
			switch strings.ToLower(strings.TrimSpace(part)) {
			case "", "tcp", "udp", "udp_multicast":
			default:
				return fmt.Errorf("output target %q asks for RTSP transport %q, which has no connection kind in the push table",
					o.target, strings.TrimSpace(part))
			}
		}
		return nil
	}
	if _, ok := pushStreamKind(scheme); ok {
		return nil
	}
	return fmt.Errorf("output target %q uses protocol family %q, which has no connection kind in the push table",
		o.target, scheme)
}

// fecEnabled reports whether the request asked for Pro-MPEG FEC. ffmpeg only
// accepts `-fec prompeg` on the rtp_mpegts muxer, and derives the two FEC
// streams' ports from the RTP URL port (+2 for the column stream, +4 for the
// row stream), so the relays have to reserve that whole block.
func (o jobOutput) fecEnabled(existingArgs []string) bool {
	value := ffmpegOptionValue(outputSideFFmpegArgs(existingArgs), "-fec")
	if value == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "prompeg")
}

// isRTP reports whether the muxer opens the RTP and RTCP datagram pair rather
// than a single UDP socket.
func (o jobOutput) isRTP() bool {
	scheme := pushTargetScheme(o.target)
	return scheme == "rtp" || scheme == "rtp_mpegts"
}

// rtpPorts returns the remote RTP and RTCP ports ffmpeg would use. RTCP
// defaults to the RTP port plus one and is overridden by either accepted
// spelling of the query option.
func (o jobOutput) rtpPorts() (int, int, error) {
	parsed, err := url.Parse(o.target)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid output target %q: %w", o.target, err)
	}
	port := parsed.Port()
	if port == "" {
		return 0, 0, fmt.Errorf("output target %q carries no RTP port", o.target)
	}
	rtpPort, err := parsePushPort(port)
	if err != nil {
		return 0, 0, fmt.Errorf("output target %q: %w", o.target, err)
	}
	rtcpPort := rtpPort + 1
	values := parsed.Query()
	for _, name := range []string{"rtcpport", "rtcp_port"} {
		if raw := values[name]; len(raw) > 0 {
			rtcpPort, err = parsePushPort(raw[len(raw)-1])
			if err != nil {
				return 0, 0, fmt.Errorf("output target %q: %w", o.target, err)
			}
			break
		}
	}
	if rtcpPort > 65535 {
		return 0, 0, fmt.Errorf("output target %q: RTCP port %d is out of range", o.target, rtcpPort)
	}
	return rtpPort, rtcpPort, nil
}

func parsePushPort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %q is not between 1 and 65535", value)
	}
	return port, nil
}

// rtpTargetAtHost gives ffmpeg the two relay sockets as the RTP and RTCP
// destinations. The query keeps every other option and replaces both accepted
// spellings of the RTCP port with the local relay port.
func (o jobOutput) rtpTargetAtHost(rtpHost, rtcpHost string) (string, error) {
	parsed, err := url.Parse(o.target)
	if err != nil {
		return "", fmt.Errorf("invalid output target %q: %w", o.target, err)
	}
	_, rtcpPort, err := net.SplitHostPort(rtcpHost)
	if err != nil {
		return "", fmt.Errorf("invalid RTCP relay address %q: %w", rtcpHost, err)
	}
	parsed.Host = rtpHost
	parts := strings.Split(parsed.RawQuery, "&")
	kept := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		if part == "" {
			continue
		}
		name := part
		if i := strings.IndexByte(part, '='); i >= 0 {
			name = part[:i]
		}
		if name == "rtcpport" || name == "rtcp_port" {
			continue
		}
		kept = append(kept, part)
	}
	kept = append(kept, "rtcpport="+rtcpPort)
	parsed.RawQuery = strings.Join(kept, "&")
	return parsed.String(), nil
}

// headerHasHost reports whether a request-supplied header block already names
// a Host header, in which case that explicit value is preserved.
func headerHasHost(headers string) bool {
	for _, line := range strings.FieldsFunc(headers, func(r rune) bool { return r == '\n' }) {
		name := strings.TrimSpace(line)
		if len(name) >= 5 && strings.EqualFold(name[:5], "host:") {
			return true
		}
	}
	return false
}

// rtmpAppFromPath mirrors the native RTMP parser's app/playpath split
// (libavformat/rtmpproto.c): the app keeps the path prefix before the last
// segment, including an instance segment, so /live/instance/key belongs to
// app live/instance rather than live. A colon marks a playpath when it appears
// before the next slash, and slist= keeps the whole path as the app.
func rtmpAppFromPath(path string) string {
	if qmark := strings.IndexByte(path, '?'); qmark >= 0 && strings.Contains(path[qmark:], "slist=") {
		return strings.TrimPrefix(path, "/")
	}
	if strings.HasPrefix(path, "/ondemand/") {
		return "ondemand"
	}
	next := path
	if next != "" {
		next = next[1:]
	}
	firstSlash := strings.IndexByte(next, '/')
	if firstSlash < 0 {
		return next
	}
	rest := next[firstSlash+1:]
	colon := strings.IndexByte(rest, ':')
	nextSlash := strings.IndexByte(rest, '/')
	if nextSlash < 0 || (colon >= 0 && colon < nextSlash) {
		return path[1 : 1+firstSlash]
	}
	return path[1 : 1+firstSlash+1+nextSlash]
}

// outputSideFFmpegArgs returns the arguments ffmpeg interprets as output
// options: everything after the last input operand. A protocol option before an
// input belongs to that input and must not suppress an output default.
func outputSideFFmpegArgs(args []string) []string {
	lastInput := -1
	for i, arg := range args {
		if arg == "-i" && i+1 < len(args) {
			lastInput = i + 1
		}
	}
	if lastInput < 0 {
		return args
	}
	return args[lastInput+1:]
}

// hasFFmpegOption reports whether the request set an option itself. An explicit
// value is the request's decision and is not duplicated.
func hasFFmpegOption(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

// ffmpegOptionValue returns the last value the request gave an option.
func ffmpegOptionValue(args []string, name string) string {
	value := ""
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			value = args[i+1]
			continue
		}
		if strings.HasPrefix(args[i], name+"=") {
			value = strings.TrimPrefix(args[i], name+"=")
		}
	}
	return value
}

// rewriteOutputTargetArg returns a copy of args with the push target replaced
// and the protocol options placed immediately before it. The output target is
// the last argument carrying the original value; a request may repeat the URL
// earlier in its argument list, and only the output position may be rewritten.
// Options stay before a "--" separator so ffmpeg still parses them.
func rewriteOutputTargetArg(args []string, original, replacement string, options []string) ([]string, bool) {
	targetIdx := -1
	for i := len(args) - 1; i >= 0; i-- {
		if args[i] == original {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return args, false
	}
	insertAt := targetIdx
	if insertAt > 0 && args[insertAt-1] == "--" {
		insertAt--
	}
	rewritten := make([]string, 0, len(args)+len(options))
	rewritten = append(rewritten, args[:insertAt]...)
	rewritten = append(rewritten, options...)
	rewritten = append(rewritten, args[insertAt:]...)
	rewritten[targetIdx+len(options)] = replacement
	return rewritten, true
}

// planJobOutput judges the output target before anything is written (C-125 /
// C-129). It returns the single file name to materialise inside the workspace,
// the absolute target of a pass-through job, the stdout pipe, or the protocol
// address a network output writes to. Rules that only the execution side can
// judge — the resolved pass-through target landing outside the declared space,
// or the resolved push target landing in a denied address class — fail the job
// here instead of connecting or writing anywhere.
func (w *Worker) planJobOutput(ctx context.Context, job protocol.JobInfo, directMode bool) (jobOutput, *boundaryFailure) {
	if job.StreamingOutput {
		// The target is the stdout pipe, not a filesystem path (C-7).
		return jobOutput{target: "-", form: pathutil.OutputTargetFile}, nil
	}
	name := job.OutputFilename
	if directMode {
		if v := w.validateDirectOutputPath(name); v != nil {
			return jobOutput{}, &boundaryFailure{msg: v.msg, details: v.details}
		}
		return jobOutput{target: name, form: pathutil.OutputTargetFile}, nil
	}
	if name == "" {
		// Legacy rows created before the output-name rule existed: the default
		// is a workspace-internal name, so it cannot point outside the job.
		name = "output"
	}
	form, err := pathutil.ClassifyOutputTarget(name, false, w.pathSpace())
	if err != nil {
		return jobOutput{}, &boundaryFailure{
			msg:     "output name is neither a single file name nor a protocol address: " + name,
			details: err.Error(),
		}
	}
	if form == pathutil.OutputTargetProtocolAddress {
		// C-129: the whole target is judged by its resolved address before
		// ffmpeg starts, so a denied class never receives a connection. A
		// target whose address cannot be determined is refused as well
		// (fail-closed): the judgement would be on an address nobody vouched
		// for. The returned set is the only place ffmpeg may connect to.
		addresses, judgeErr := addrpolicy.CheckOutputTarget(ctx, name)
		if judgeErr != nil {
			return jobOutput{}, &boundaryFailure{
				msg:     "output target address is not permitted: " + name,
				details: judgeErr.Error(),
			}
		}
		return jobOutput{target: name, form: form, addresses: addresses}, nil
	}
	return jobOutput{target: name, form: form}, nil
}

// inputLandingPath returns the workspace path a downloaded input is written to.
// The name is derived from the input's position, never from the identifier text
// (C-126), and the result is verified to stay inside the workspace.
func inputLandingPath(jobDir string, index int) (string, bool) {
	landing := filepath.Join(jobDir, fmt.Sprintf("input-%d", index))
	return landing, pathutil.Within(jobDir, landing)
}
