package worker

import (
	"encoding/json"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// captureFFmpegArgs writes an ffmpeg stand-in that records every argument it
// receives and exits 0, so a network-output job can be proven to hand the
// protocol address to ffmpeg verbatim instead of writing a local file.
func captureFFmpegArgs(t *testing.T, argsFile string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "ffmpeg-capture-args")
	body := "printf '%s\\n' \"$@\" > " + shellSingleQuote(argsFile) + "\nexit 0\n"
	if err := writeShellFixture(script, body); err != nil {
		t.Fatalf("write capture ffmpeg: %v", err)
	}
	return script
}

// reachablePushTarget builds a protocol target whose literal address accepts a
// TCP connection. The relay dials it only when ffmpeg connects, so the fixture
// stays a real judged landing.
func reachablePushTarget(t *testing.T, scheme string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("start protocol target listener: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("list interface addresses: %v", err)
	}
	for _, raw := range interfaces {
		prefix, err := netip.ParsePrefix(raw.String())
		if err != nil {
			continue
		}
		addr := prefix.Addr()
		if addr.Is4() && addr.IsPrivate() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast() {
			return scheme + "://" + net.JoinHostPort(addr.String(), strconv.Itoa(port)) + "/live/stream"
		}
	}
	t.Skip("no private host address is available for a reachable protocol target")
	return ""
}

// TestProcessJobProtocolTargetIsHandedToFFmpeg covers the execution side of
// C-125 / C-129: ffmpeg receives the worker-owned relay address instead of a
// resolver-dependent host name, the request host survives in the protocol's
// session name, no local output file exists afterwards, nothing is uploaded
// and the cache is bypassed (C-96).
func TestProcessJobProtocolTargetIsHandedToFFmpeg(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	w.executor = NewExecutor(captureFFmpegArgs(t, argsFile), 30*time.Second)

	target := reachablePushTarget(t, "rtmp")
	job := protocol.JobInfo{
		ID:             "protocol-target-001",
		InputFiles:     []string{testUploadID("protocol-target")},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-f", "flv"},
		OutputFilename: target,
	}
	runJob(t, w, job)

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ffmpeg never ran for a protocol address target: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(args) == 0 {
		t.Fatal("ffmpeg received no output argument")
	}
	outputArg := args[len(args)-1]
	parsed, err := url.Parse(outputArg)
	if err != nil {
		t.Fatalf("ffmpeg output argument %q is not a URL: %v", outputArg, err)
	}
	if parsed.Scheme != "rtmp" || parsed.Path != "/live/stream" {
		t.Fatalf("ffmpeg output argument = %q, want rtmp://<relay>/live/stream", outputArg)
	}
	if host := parsed.Hostname(); host != "127.0.0.1" {
		t.Fatalf("ffmpeg output host = %q, want the worker relay (the judged address is dialed by the relay)", host)
	}
	request, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	wantTCURL := "rtmp://" + request.Host + "/live"
	if got := ffmpegOptionValue(args, "-rtmp_tcurl"); got != wantTCURL {
		t.Errorf("rtmp_tcurl = %q, want the request host %q", got, wantTCURL)
	}
	for _, arg := range args {
		if filepath.IsAbs(arg) && strings.HasSuffix(arg, "rtmp:") {
			t.Errorf("ffmpeg received a rewritten local path instead of the address: %q", arg)
		}
	}

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(mockSrv.lastUpdateBody, &update); err != nil {
		t.Fatalf("decode terminal update: %v", err)
	}
	if update.Status != protocol.JobStatusCompleted {
		t.Errorf("job status = %q, want %q (error: %s)", update.Status, protocol.JobStatusCompleted, update.Error)
	}
	if got := mockSrv.UploadCount(); got != 0 {
		t.Errorf("uploaded %d output file(s) for a network output, want 0", got)
	}
	if got := w.cache.Stats().EntryCount; got != 0 {
		t.Errorf("cache holds %d entry for a network output, want 0", got)
	}
}

// TestProcessJobDeniedProtocolTargetFailsBeforeFFmpeg covers the execution-side
// address judgement of C-129: a host name resolving into a denied class fails
// the job as INFRA before ffmpeg starts, so the target receives no connection.
func TestProcessJobDeniedProtocolTargetFailsBeforeFFmpeg(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)
	marker := filepath.Join(t.TempDir(), "ffmpeg-ran")
	script := filepath.Join(t.TempDir(), "ffmpeg-marker")
	if err := writeShellFixture(script, "touch "+shellSingleQuote(marker)+"\nexit 0\n"); err != nil {
		t.Fatalf("write marker ffmpeg: %v", err)
	}
	w.executor = NewExecutor(script, 30*time.Second)

	job := protocol.JobInfo{
		ID:             "protocol-target-denied",
		InputFiles:     []string{testUploadID("protocol-target-denied")},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-f", "flv"},
		OutputFilename: "rtmp://localhost:1935/live/stream",
	}
	runJob(t, w, job)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ffmpeg ran for a job whose push target resolves into a denied address class")
	}
	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(mockSrv.lastUpdateBody, &update); err != nil {
		t.Fatalf("decode terminal update: %v", err)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("job status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("failure_type = %q, want %q", update.FailureType, protocol.FailureInfra)
	}
	if !strings.Contains(update.FailureDetails, "not permitted") {
		t.Errorf("failure_details = %q, want the address-policy refusal", update.FailureDetails)
	}
	if got := mockSrv.UploadCount(); got != 0 {
		t.Errorf("uploaded %d output file(s) for a denied target, want 0", got)
	}
}
