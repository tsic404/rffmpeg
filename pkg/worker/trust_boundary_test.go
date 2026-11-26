package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/addrpolicy"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// disabledRewriteAdapter is a rewrite adapter that passes the request through
// unchanged, so fixtures observe the submitted arguments verbatim.
func disabledRewriteAdapter() *RewriteAdapter {
	a := NewRewriteAdapter()
	a.config.Enabled = false
	return a
}

// TestDeniedRemoteClass covers the C-128 address classes: the listed classes
// are refused, private space and ordinary global unicast are allowed, and an
// address that is not listed is allowed by default (AC-59 boundary).
func TestDeniedRemoteClass(t *testing.T) {
	denied := []struct {
		addr  string
		class string
	}{
		{"127.0.0.1", "loopback"},
		{"127.255.255.254", "loopback"},
		{"::1", "loopback"},
		{"::ffff:127.0.0.1", "loopback"},
		{"169.254.1.1", "link-local"},
		{"fe80::1", "link-local"},
		{"100.64.0.1", "shared address space"},
		{"0.0.0.0", "this-network"},
		{"::", "unspecified"},
		{"224.0.0.1", "multicast"},
		{"239.255.255.250", "multicast"},
		{"ff02::1", "multicast"},
		{"255.255.255.255", "reserved"},
		{"240.0.0.1", "reserved"},
		{"192.0.2.5", "documentation"},
		{"198.51.100.9", "documentation"},
		{"203.0.113.9", "documentation"},
		{"2001:db8::1", "documentation"},
		{"198.18.0.1", "benchmarking"},
	}
	for _, tc := range denied {
		got := addrpolicy.DeniedClass(netip.MustParseAddr(tc.addr))
		if got != tc.class {
			t.Errorf("DeniedClass(%s) = %q, want %q", tc.addr, got, tc.class)
		}
	}

	allowed := []string{
		"10.0.0.1",             // RFC1918
		"172.16.5.4",           // RFC1918
		"172.31.255.254",       // RFC1918 upper bound
		"192.168.1.10",         // RFC1918
		"fd00::1",              // IPv6 unique local
		"fc00::abcd",           // IPv6 unique local
		"8.8.8.8",              // ordinary global unicast
		"1.1.1.1",              // ordinary global unicast
		"2001:4860:4860::8888", // ordinary global unicast (not listed)
	}
	for _, raw := range allowed {
		if class := addrpolicy.DeniedClass(netip.MustParseAddr(raw)); class != "" {
			t.Errorf("DeniedClass(%s) = %q, want allowed", raw, class)
		}
	}
}

// TestRemoteInputClientRefusesDeniedTargetBeforeConnect proves the policy sits
// ahead of the connection: a request to a denied literal address never reaches
// the fixture server.
func TestRemoteInputClientRefusesDeniedTargetBeforeConnect(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := addrpolicy.NewRemoteInputClient(5 * time.Second)
	resp, err := client.Get(srv.URL + "/input.mp4")
	if err == nil {
		resp.Body.Close()
		t.Fatal("a loopback target was fetched, want a refusal")
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("error = %v, want an address-policy refusal", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("denied target received %d request(s), want 0", got)
	}
}

// TestRemoteInputClientJudgesRedirectHops proves the redirect hook applies the
// same judgement to every hop instead of following blindly.
func TestRemoteInputClientJudgesRedirectHops(t *testing.T) {
	client := addrpolicy.NewRemoteInputClient(5 * time.Second)
	if client.CheckRedirect == nil {
		t.Fatal("remote input client has no redirect judgement")
	}
	req, err := http.NewRequest("GET", "http://127.0.0.1:9/hop.mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(req, nil); err == nil {
		t.Fatal("a redirect into loopback was allowed")
	}
}

// TestPlanJobInputsRejectsUnclassifiedIdentifier locks the C-124 landing: the
// execution side refuses an identifier that matches no form before anything is
// written.
func TestPlanJobInputsRejectsUnclassifiedIdentifier(t *testing.T) {
	root := t.TempDir()
	w := &Worker{sharedFS: pathutil.NewSpace(root)}

	for _, identifier := range []string{
		"input-001",                         // neither hex, URL nor absolute path
		"0123456789abcdef?a://../../etc/x",  // URL-like substring inside a hex prefix
		"ftp://files.example.com/media.avi", // scheme outside the input taxonomy
	} {
		_, failure := w.planJobInputs(protocol.JobInfo{InputFiles: []string{identifier}}, false)
		if failure == nil {
			t.Errorf("planJobInputs(%q) accepted an unclassified identifier", identifier)
		}
	}
}

// TestPlanJobOutputNames locks the C-125 non-pass-through shapes: a single file
// name, or a protocol address target; the empty name of a legacy row stays a
// workspace name.
func TestPlanJobOutputNames(t *testing.T) {
	w := &Worker{sharedFS: pathutil.NewSpace(""), tempDir: t.TempDir()}
	ctx := context.Background()

	if got, failure := w.planJobOutput(ctx, protocol.JobInfo{OutputFilename: "out.mp4"}, false); failure != nil ||
		got.target != "out.mp4" || got.form != pathutil.OutputTargetFile {
		t.Errorf("planJobOutput(out.mp4) = %+v, %v; want the workspace file name", got, failure)
	}
	if got, failure := w.planJobOutput(ctx, protocol.JobInfo{}, false); failure != nil ||
		got.target != "output" || got.form != pathutil.OutputTargetFile {
		t.Errorf("planJobOutput(empty) = %+v, %v; want the legacy default", got, failure)
	}
	for _, name := range []string{"../escape.mp4", "sub/out.mp4", "/etc/passwd", "a/b", "file:///etc/passwd", "rtmp://"} {
		if _, failure := w.planJobOutput(ctx, protocol.JobInfo{OutputFilename: name}, false); failure == nil {
			t.Errorf("planJobOutput(%q) accepted a target that is neither a file name nor a protocol address", name)
		}
	}
	// A protocol address target is classified as such and kept verbatim; a
	// private address is on the allowed side (C-129 reuses C-128's classes).
	got, failure := w.planJobOutput(ctx, protocol.JobInfo{OutputFilename: "rtmp://10.20.30.40:1935/live/x"}, false)
	if failure != nil || got.target != "rtmp://10.20.30.40:1935/live/x" ||
		got.form != pathutil.OutputTargetProtocolAddress {
		t.Errorf("planJobOutput(private protocol target) = %+v, %v; want the address kept verbatim", got, failure)
	}
	if len(got.addresses) != 1 || got.addresses[0] != netip.MustParseAddr("10.20.30.40") {
		t.Errorf("planJobOutput(private protocol target) addresses = %v, want [10.20.30.40]", got.addresses)
	}
	// A denied class fails the job before ffmpeg starts.
	for _, name := range []string{
		"rtmp://127.0.0.1:1935/live/x",   // loopback literal
		"udp://169.254.169.254:9/stream", // link-local literal
		"rtmp://[::1]:1935/live/x",       // IPv6 loopback literal
		"udp://localhost:9000/stream",    // host name resolving to loopback
		// inet_aton numeric spellings: the C library ffmpeg resolves with maps
		// these to the same denied addresses.
		"udp://127.1:9000/stream",
		"udp://2130706433:9000/stream",
		"udp://0x7f000001:9000/stream",
		"udp://0xa9fea9fe:9/stream",
		// A target whose address cannot be judged is refused (fail-closed).
		"udp://no-such-host.invalid:9000/stream",
	} {
		if _, failure := w.planJobOutput(ctx, protocol.JobInfo{OutputFilename: name}, false); failure == nil {
			t.Errorf("planJobOutput(%q) accepted a denied push target", name)
		}
	}
}

// TestJobOutputTargetAtPinsJudgedAddress locks the C-129 landing handed to
// ffmpeg: the judged address replaces the host name, and everything else in the
// target (user info, port, path, query) survives.
func TestJobOutputTargetAtPinsJudgedAddress(t *testing.T) {
	out := jobOutput{
		target: "rtmp://publisher:pw@stream.example.test:1935/live/x?token=abc",
		form:   pathutil.OutputTargetProtocolAddress,
	}
	got, err := out.targetAt(netip.MustParseAddr("10.20.30.40"))
	if err != nil {
		t.Fatalf("targetAt: %v", err)
	}
	want := "rtmp://publisher:pw@10.20.30.40:1935/live/x?token=abc"
	if got != want {
		t.Errorf("targetAt = %q, want %q", got, want)
	}

	ipv6 := jobOutput{target: "srt://stream.example.test:9000?mode=caller", form: pathutil.OutputTargetProtocolAddress}
	got, err = ipv6.targetAt(netip.MustParseAddr("fd00::7"))
	if err != nil {
		t.Fatalf("targetAt ipv6: %v", err)
	}
	want = "srt://[fd00::7]:9000?mode=caller"
	if got != want {
		t.Errorf("targetAt ipv6 = %q, want %q", got, want)
	}
}

// TestJobOutputTargetAtHostKeepsRequestShape locks the relay landing: only the
// host changes, while user info, port, path and query survive.
func TestJobOutputTargetAtHostKeepsRequestShape(t *testing.T) {
	out := jobOutput{target: "rtmp://publisher:pw@stream.example.test:1935/live/x?token=abc"}
	got, err := out.targetAtHost("127.0.0.1:41234")
	if err != nil {
		t.Fatalf("targetAtHost: %v", err)
	}
	want := "rtmp://publisher:pw@127.0.0.1:41234/live/x?token=abc"
	if got != want {
		t.Errorf("targetAtHost = %q, want %q", got, want)
	}
}

// TestRewriteOutputTargetArgReplacesOnlyTheOutputPosition proves a URL repeated
// earlier in the request (a filter value, say) is not mistaken for the output.
func TestRewriteOutputTargetArgReplacesOnlyTheOutputPosition(t *testing.T) {
	original := "tcp://stream.example.test:9000/live"
	args := []string{"-i", "in.mp4", "-metadata", "comment=" + original, "-f", "mpegts", original}
	replaced, ok := rewriteOutputTargetArg(args, original, "tcp://10.0.0.7:9000/live", nil)
	if !ok {
		t.Fatal("rewriteOutputTargetArg did not find the output target")
	}
	if replaced[len(replaced)-1] != "tcp://10.0.0.7:9000/live" {
		t.Errorf("output argument = %q, want the judged address", replaced[len(replaced)-1])
	}
	if replaced[3] != "comment="+original {
		t.Errorf("earlier argument = %q, want it untouched", replaced[3])
	}
	if args[len(args)-1] != original {
		t.Error("rewriteOutputTargetArg mutated the caller's slice")
	}
}

// TestRewriteOutputTargetArgKeepsOptionsBeforeSeparator proves protocol options
// remain options when the request used "--" to end option parsing.
func TestRewriteOutputTargetArgKeepsOptionsBeforeSeparator(t *testing.T) {
	original := "http://stream.example.test:8080/live"
	args := []string{"-i", "in.mp4", "-f", "mpegts", "--", original}
	replaced, ok := rewriteOutputTargetArg(args, original,
		"http://10.0.0.7:8080/live", []string{"-max_redirects", "0"})
	if !ok {
		t.Fatal("rewriteOutputTargetArg did not find the output target")
	}
	want := []string{"-i", "in.mp4", "-f", "mpegts", "-max_redirects", "0", "--", "http://10.0.0.7:8080/live"}
	if strings.Join(replaced, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("rewritten args = %q, want %q", replaced, want)
	}
}

// TestPushOutputOptions locks the protocol-level carriers C-129 needs: HTTP
// redirects are refused so no second connection leaves the judged set, and RTMP
// keeps the requested host name in tcUrl while the URL carries the judged IP.
func TestPushOutputOptions(t *testing.T) {
	httpOut := jobOutput{target: "https://stream.example.test:8443/live"}
	wantHost := "-max_redirects 0 -headers Host: stream.example.test:8443\r\n"
	if got := strings.Join(httpOut.ffmpegOutputOptions(nil), " "); got != wantHost {
		t.Errorf("https output options = %q, want %q", got, wantHost)
	}
	// A request-supplied redirect budget must not survive: the option inserted
	// at the output position is the last one, so redirects stay refused.
	if got := strings.Join(httpOut.ffmpegOutputOptions([]string{"-max_redirects", "8"}), " "); got != wantHost {
		t.Errorf("https output options with request redirects = %q, want %q", got, wantHost)
	}
	// A request that already carries custom headers keeps them in full; the
	// worker's -headers value is the last one ffmpeg sees, so it repeats them
	// and adds the request Host.
	customHeaders := []string{"-headers", "X-Test: 1\r\n"}
	wantCustom := "-max_redirects 0 -headers X-Test: 1\r\nHost: stream.example.test:8443\r\n"
	if got := strings.Join(httpOut.ffmpegOutputOptions(customHeaders), " "); got != wantCustom {
		t.Errorf("https output options with custom headers = %q, want %q", got, wantCustom)
	}
	// An explicit Host chosen by the request is not overridden.
	explicitHost := []string{"-headers", "X-Test: 1\r\nHost: edge.example.test\r\n"}
	if got := strings.Join(httpOut.ffmpegOutputOptions(explicitHost), " "); got != "-max_redirects 0" {
		t.Errorf("https output options with an explicit Host = %q, want only the redirect refusal", got)
	}
	explicitHostNoSpace := []string{"-headers", "X-Test: 1\r\nHost:edge.example.test\r\n"}
	if got := strings.Join(httpOut.ffmpegOutputOptions(explicitHostNoSpace), " "); got != "-max_redirects 0" {
		t.Errorf("https output options with a compact Host = %q, want only the redirect refusal", got)
	}
	// Headers set before the last input belong to the input, not the output.
	inputHeaders := []string{"-headers", "X-Input: 1\r\n", "-i", "https://input.example.test/live"}
	if got := strings.Join(httpOut.ffmpegOutputOptions(inputHeaders), " "); got != wantHost {
		t.Errorf("https output options with input-side headers = %q, want %q", got, wantHost)
	}

	rtmpOut := jobOutput{target: "rtmp://stream.example.test:1935/live/key"}
	if got := strings.Join(rtmpOut.ffmpegOutputOptions(nil), " "); got != "-rtmp_tcurl rtmp://stream.example.test:1935/live" {
		t.Errorf("rtmp output options = %q, want the requested host in tcUrl", got)
	}

	instanceOut := jobOutput{target: "rtmp://stream.example.test:1935/live/instance/key"}
	if got := strings.Join(instanceOut.ffmpegOutputOptions(nil), " "); got != "-rtmp_tcurl rtmp://stream.example.test:1935/live/instance" {
		t.Errorf("rtmp instance output options = %q, want app live/instance in tcUrl", got)
	}

	explicit := []string{"-rtmp_tcurl", "rtmp://explicit.example.test/app"}
	if got := rtmpOut.ffmpegOutputOptions(explicit); got != nil {
		t.Errorf("explicit -rtmp_tcurl was duplicated: %q", got)
	}

	inputOnly := []string{"-rtmp_tcurl", "rtmp://source.example.test/live", "-i", "rtmp://source.example.test/live"}
	if got := strings.Join(rtmpOut.ffmpegOutputOptions(inputOnly), " "); got != "-rtmp_tcurl rtmp://stream.example.test:1935/live" {
		t.Errorf("input-side -rtmp_tcurl suppressed the output default: %q", got)
	}

	appOverride := []string{"-rtmp_app", "live2"}
	if got := strings.Join(rtmpOut.ffmpegOutputOptions(appOverride), " "); got != "-rtmp_tcurl rtmp://stream.example.test:1935/live2" {
		t.Errorf("rtmp output options with -rtmp_app = %q, want app live2", got)
	}

	// Plain RTSP keeps the request's transport choice: the control relay
	// rewrites SETUP so negotiated UDP media still lands on the judged IP.
	rtspOut := jobOutput{target: "rtsp://stream.example.test:8554/live"}
	if got := rtspOut.ffmpegOutputOptions([]string{"-rtsp_transport", "udp"}); got != nil {
		t.Errorf("explicit RTSP UDP was overridden: %q", got)
	}
	if got := rtspOut.ffmpegOutputOptions(nil); got != nil {
		t.Errorf("RTSP output options = %q, want the request's default transport", got)
	}
	// Encrypted RTSP leaves the transport to the request. ffmpeg pins rtsps to
	// interleaved TCP itself, so the worker must neither force TCP nor refuse
	// an explicit UDP request (C-133); the media then rides the judged control
	// connection (C-137).
	rtspsOut := jobOutput{target: "rtsps://stream.example.test:322/live"}
	if got := rtspsOut.ffmpegOutputOptions(nil); got != nil {
		t.Errorf("rtsps default output options = %q, want the request's default transport", got)
	}
	for _, value := range []string{"udp", "udp_multicast", "tcp", "tcp+udp"} {
		args := []string{"-rtsp_transport", value}
		if got := rtspsOut.ffmpegOutputOptions(args); got != nil {
			t.Errorf("rtsps -rtsp_transport %s was rewritten: %q", value, got)
		}
		if err := rtspsOut.pushKindError(args); err != nil {
			t.Errorf("rtsps -rtsp_transport %s was refused: %v", value, err)
		}
	}

	tcpOut := jobOutput{target: "tcp://stream.example.test:9000/live"}
	if got := tcpOut.ffmpegOutputOptions(nil); got != nil {
		t.Errorf("tcp output options = %q, want none", got)
	}
}

// TestRTMPAppFromPath mirrors the native app/playpath split: the segment
// before the last slash is part of the app, while a colon marks a playpath.
func TestRTMPAppFromPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/live/key", "live"},
		{"/live/instance/key", "live/instance"},
		{"/live/instance", "live"},
		{"/live/instance/key/", "live/instance"},
		{"/live/instance:key", "live"},
		{"/live/instance/key:foo", "live/instance"},
		{"/live/", "live"},
		{"/key", "key"},
		{"/onnection", "onnection"},
		{"/ondemand/movie", "ondemand"},
		{"/app?token=x&slist=stream", "app?token=x&slist=stream"},
		{"", ""},
	} {
		if got := rtmpAppFromPath(tc.path); got != tc.want {
			t.Errorf("rtmpAppFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestJobOutputRequestHost(t *testing.T) {
	out := jobOutput{target: "rtmpt://tc15-rtmpt-host.test:8080/live/key"}
	if got := out.requestHost(); got != "tc15-rtmpt-host.test:8080" {
		t.Fatalf("requestHost = %q", got)
	}
}

// TestJobArgumentsOutsideTheManifestFailBeforeFFmpeg covers the execution-side
// C-127 landing: the worker refuses the job before starting ffmpeg and reports
// INFRA, so no product exists.
func TestJobArgumentsOutsideTheManifestFailBeforeFFmpeg(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input.mp4")
	if err := os.WriteFile(input, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "ffmpeg-ran")
	script := filepath.Join(root, "ffmpeg")
	if err := writeShellFixture(script, "touch '"+marker+"'\nexit 0\n"); err != nil {
		t.Fatal(err)
	}

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	executor := NewExecutor(script, 30*time.Second)
	w := &Worker{
		id:              "test-worker-001",
		client:          NewClient(mockSrv.URL, "test-worker-001", ""),
		executor:        executor,
		retryExecutor:   NewRetryExecutor(executor, DefaultRetryConfig()),
		rewriteAdapter:  NewRewriteAdapter(),
		tempDir:         t.TempDir(),
		sharedFS:        pathutil.NewSpace(root),
		ffprobeExecutor: NewFFprobeExecutor("false"),
		gpuDetector:     gpu.NewDetector(),
	}

	job := protocol.JobInfo{
		ID:             "args-boundary-001",
		InputFiles:     []string{input},
		DirectPaths:    []string{input},
		Args:           []string{"-i", "<INPUT_FILE>", "-filter_complex", "movie=/etc/passwd", "-c:v", "libx264"},
		OutputFilename: filepath.Join(root, "out.mp4"),
	}
	w.processJob(context.Background(), job, func() {}, false)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ffmpeg ran for a job whose arguments reach outside the manifest")
	}
	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal update for the rejected job")
	}
	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()
	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("decode terminal update: %v (body %s)", err, body)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want failed", update.Status)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("FailureType = %q, want INFRA", update.FailureType)
	}
	if !strings.Contains(update.Error, "argument") {
		t.Errorf("Error = %q, want it to name the offending argument", update.Error)
	}
	if _, err := os.Stat(filepath.Join(root, "out.mp4")); err == nil {
		t.Error("a rejected job produced an output file")
	}
}

// TestDownloadedInputLandingNameIsPositional covers the C-126 landing rule: the
// downloaded input is materialised from its position, never from the identifier
// text, and the identifier's own spelling does not appear in the path.
func TestDownloadedInputLandingNameIsPositional(t *testing.T) {
	root := t.TempDir()
	argvFile := filepath.Join(root, "argv.txt")
	script := filepath.Join(root, "ffmpeg")
	if err := writeShellFixture(script, "printf '%s\\n' \"$@\" > '"+argvFile+"'\nexit 0\n"); err != nil {
		t.Fatal(err)
	}

	mockSrv := newMockJobServer()
	defer mockSrv.Close()

	cache, err := NewCache(CacheConfig{Enabled: false, Dir: filepath.Join(t.TempDir(), "cache"), TTL: time.Hour})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	t.Cleanup(cache.Stop)

	executor := NewExecutor(script, 30*time.Second)
	w := &Worker{
		id:              "test-worker-001",
		client:          NewClient(mockSrv.URL, "test-worker-001", ""),
		executor:        executor,
		retryExecutor:   NewRetryExecutor(executor, DefaultRetryConfig()),
		rewriteAdapter:  disabledRewriteAdapter(),
		cache:           cache,
		tempDir:         filepath.Join(root, "worker-temp"),
		activeJobs:      make(map[string]context.CancelFunc),
		ffprobeExecutor: NewFFprobeExecutor("false"),
		gpuDetector:     gpu.NewDetector(),
	}

	identifier := testUploadID("positional")
	job := protocol.JobInfo{
		ID:             "landing-001",
		InputFiles:     []string{identifier},
		Args:           []string{"-i", "<INPUT_FILE>", "out.mp4"},
		OutputFilename: "out.mp4",
	}
	w.processJob(context.Background(), job, func() {}, false)

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ffmpeg fixture did not run: %v (terminal update: %s)", err, mockSrv.lastUpdateBody)
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	inputArg := ""
	for i, arg := range argv {
		if arg == "-i" && i+1 < len(argv) {
			inputArg = argv[i+1]
		}
	}
	if inputArg == "" {
		t.Fatalf("no -i argument recorded; argv=%v", argv)
	}
	if filepath.Base(inputArg) != "input-0" {
		t.Errorf("landing name = %q, want input-0 (positional, never the identifier text)", filepath.Base(inputArg))
	}
	if strings.Contains(inputArg, identifier) {
		t.Errorf("landing path %q embeds the identifier text", inputArg)
	}
}
