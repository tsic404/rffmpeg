package worker

// Regression coverage for the encoder-fallback retry on a push job: the retry
// re-executes ffmpeg against the same judged address, so it must reach a live
// relay rather than the loopback endpoint the first pass already released.

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// writeFailingThenConnectingFFmpeg writes a stand-in that fails its first
// invocation with a retryable error and connects on the next one. Both
// invocations dial the target they are handed, so the receiver sees exactly
// which relay endpoints ffmpeg could reach.
func writeFailingThenConnectingFFmpeg(t *testing.T, dir string) string {
	t.Helper()
	counter := filepath.Join(dir, "invocations")
	trace := filepath.Join(dir, "trace")
	script := `target=""
for arg in "$@"; do target="$arg"; done
rest="${target#*://}"
hostport="${rest%%/*}"
host="${hostport%%:*}"
port="${hostport##*:}"
printf 'target=%s host=%s port=%s\n' "$target" "$host" "$port" >> ` + shellSingleQuote(trace) + `
n=0
if [ -f ` + shellSingleQuote(counter) + ` ]; then
  n=$(cat ` + shellSingleQuote(counter) + `)
fi
n=$((n+1))
printf '%s' "$n" > ` + shellSingleQuote(counter) + `
if ! exec 3<>"/dev/tcp/${host}/${port}"; then
  printf 'connect-failed n=%s\n' "$n" >> ` + shellSingleQuote(trace) + `
  exit 1
fi
printf 'rffmpeg-push-payload' >&3
sleep 0.2
exec 3>&-
if [ "$n" -eq 1 ]; then
  echo "Invalid argument" >&2
  exit 1
fi
exit 0
`
	path := writeFixture(t, dir, "ffmpeg-retry", script)
	t.Cleanup(func() {
		if traceBytes, err := os.ReadFile(trace); err == nil {
			t.Logf("fixture trace:\n%s", traceBytes)
		}
	})
	return path
}

func TestPushRetryReachesJudgedAddressAfterFirstAttemptReleased(t *testing.T) {
	addr := privateIPv4(t)
	listener, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen on judged address: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	target := "rtmp://" + net.JoinHostPort(addr.String(), strconv.Itoa(port)) + "/live/stream"

	var accepted atomic.Int64
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			_, _ = conn.Read(make([]byte, 64))
			conn.Close()
		}
	}()

	stack := newPushE2EStack(t)
	w := newFailureMatrixWorker(t, writeFailingThenConnectingFFmpeg(t, t.TempDir()), stack.server.URL, t.TempDir())
	job := testPushJob(t, stack, "push-retry", target, testUploadID("push-retry"))

	runJob(t, w, job)

	if got := accepted.Load(); got != 2 {
		t.Fatalf("judged receiver accepted %d connections, want 2 (initial attempt plus retry)", got)
	}
	info := stack.getJob(t, job.ID)
	if info.Status != protocol.JobStatusCompleted {
		t.Fatalf("status = %s, want completed", info.Status)
	}
	if len(info.PushConnections) == 0 {
		t.Fatal("push_connections is empty, want the attempt records")
	}
	for _, element := range info.PushConnections {
		if element.Family != "rtmp" || element.Purpose != "session" {
			t.Fatalf("push_connections element = %+v, want an rtmp session", element)
		}
		if element.Target != net.JoinHostPort(addr.String(), strconv.Itoa(port)) {
			t.Fatalf("element target = %q, want the judged address", element.Target)
		}
		if element.Result != "connected" {
			t.Fatalf("element result = %q, want connected", element.Result)
		}
	}
}
