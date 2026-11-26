package worker

// End-to-end coverage for C-134/C-141/C-142: a real push attempt through the
// worker's relay produces the connection list, the list rides the terminal
// report, and GET /api/v1/jobs/{id} reads it back from the server's database.

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
)

type pushE2EStack struct {
	server   *httptest.Server
	database *db.Database
}

// newPushE2EStack wires the real job API over a real database. Job input
// downloads are served from the same test server so processJob runs its whole
// pipeline instead of a stub.
func newPushE2EStack(t *testing.T) *pushE2EStack {
	t.Helper()
	dir := t.TempDir()
	database, err := db.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	store, err := storage.New(filepath.Join(dir, "storage"))
	if err != nil {
		database.Close()
		t.Fatalf("storage.New: %v", err)
	}
	h := handlers.New(database, store, "test", workerhealth.NewWorkerStateTable(30*time.Second))

	router := chi.NewRouter()
	router.Patch("/api/v1/jobs/{jobId}", h.UpdateJob)
	router.Get("/api/v1/jobs/{jobId}", h.GetJob)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/files/") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(testMediaBytes())
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		server.Close()
		database.Close()
	})
	return &pushE2EStack{server: server, database: database}
}

func (s *pushE2EStack) getJob(t *testing.T, jobID string) protocol.JobInfo {
	t.Helper()
	resp, err := http.Get(s.server.URL + "/api/v1/jobs/" + jobID)
	if err != nil {
		t.Fatalf("GET job: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET job: status %d", resp.StatusCode)
	}
	var body protocol.JobStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode GET job response: %v", err)
	}
	return body.Job
}

// privateIPv4 returns a non-loopback private interface address, the kind the
// address policy lets a job reach and the test can bind a receiver on.
func privateIPv4(t *testing.T) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("list interface addresses: %v", err)
	}
	for _, raw := range addrs {
		prefix, err := netip.ParsePrefix(raw.String())
		if err != nil {
			continue
		}
		addr := prefix.Addr()
		if addr.Is4() && addr.IsPrivate() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast() {
			return addr
		}
	}
	t.Skip("no private host address is available for a real push target")
	return netip.Addr{}
}

// writeConnectingFFmpeg writes a stand-in that really opens the transport to
// the target it is handed (the worker's loopback relay), so the relay's dial to
// the judged address is a real attempt.
func writeConnectingFFmpeg(t *testing.T, dir, scheme string) string {
	t.Helper()
	var socket, write string
	if scheme == "udp" {
		socket = `exec 3<>"/dev/udp/${host}/${port}" || exit 1`
		write = `printf 'rffmpeg-udp-payload' >&3`
	} else {
		socket = `exec 3<>"/dev/tcp/${host}/${port}" || exit 1`
		write = `printf 'rffmpeg-push-payload' >&3`
	}
	script := "#!" + bashFixture + "\n" + `target="${!#}"
rest="${target#*://}"
hostport="${rest%%/*}"
host="${hostport%%:*}"
port="${hostport##*:}"
` + socket + "\n" + write + "\n" + `sleep 0.3
exec 3>&-
exit 0
`
	path := filepath.Join(dir, "ffmpeg-"+scheme)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write connecting ffmpeg: %v", err)
	}
	if out, err := exec.Command(bashFixture, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("fixture rejected by %s -n: %v: %s", bashFixture, err, out)
	}
	return path
}

// testPushJob creates a queued job owned by the test worker and returns the
// JobInfo the worker processes.
func testPushJob(t *testing.T, s *pushE2EStack, id, target string, inputID string) protocol.JobInfo {
	t.Helper()
	args := []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-f", "flv"}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	inputJSON, err := json.Marshal([]string{inputID})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.database.CreateJobWithStreaming(string(inputJSON), string(argsJSON), target, false, false, nil, "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.database.AssignJobToWorker(job.ID, "test-worker-001"); err != nil {
		t.Fatalf("assign job: %v", err)
	}
	return protocol.JobInfo{ID: job.ID, InputFiles: []string{inputID}, Args: args, OutputFilename: target}
}

func TestPushConnectionsRecordedFromRealRelayAndReadBack(t *testing.T) {
	addr := privateIPv4(t)
	listener, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen on judged address: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	target := "rtmp://" + net.JoinHostPort(addr.String(), strconv.Itoa(port)) + "/live/stream"

	received := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- 0
			return
		}
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Close()
		received <- n
	}()

	stack := newPushE2EStack(t)
	w := newFailureMatrixWorker(t, writeConnectingFFmpeg(t, t.TempDir(), "tcp"), stack.server.URL, t.TempDir())
	job := testPushJob(t, stack, "push-rtmp", target, testUploadID("push-rtmp"))

	runJob(t, w, job)

	if n := <-received; n == 0 {
		t.Fatal("judged receiver got no connection from the relay")
	}
	info := stack.getJob(t, job.ID)
	if info.Status != protocol.JobStatusCompleted {
		t.Fatalf("status = %s, want completed", info.Status)
	}
	if len(info.PushConnections) != 1 {
		t.Fatalf("push_connections = %+v, want one session element", info.PushConnections)
	}
	got := info.PushConnections[0]
	want := protocol.PushConnection{
		Family: "rtmp", Purpose: "session",
		Target: net.JoinHostPort(addr.String(), strconv.Itoa(port)),
		Origin: "request", Result: "connected",
	}
	if got != want {
		t.Fatalf("push_connections[0] = %+v, want %+v", got, want)
	}
	if !pushKindAllowed(got.Family, got.Purpose) {
		t.Fatalf("family/purpose %s/%s is not a C-141 combination", got.Family, got.Purpose)
	}
}

func TestPushConnectionsRecordRealDatagramFlow(t *testing.T) {
	addr := privateIPv4(t)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(addr.AsSlice()), Port: 0})
	if err != nil {
		t.Fatalf("listen on judged address: %v", err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	target := "udp://" + net.JoinHostPort(addr.String(), strconv.Itoa(port))

	var wg sync.WaitGroup
	wg.Add(1)
	gotPayload := make(chan string, 1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 64)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			gotPayload <- ""
			return
		}
		gotPayload <- string(buf[:n])
	}()

	stack := newPushE2EStack(t)
	w := newFailureMatrixWorker(t, writeConnectingFFmpeg(t, t.TempDir(), "udp"), stack.server.URL, t.TempDir())
	job := testPushJob(t, stack, "push-udp", target, testUploadID("push-udp"))

	runJob(t, w, job)
	wg.Wait()

	if payload := <-gotPayload; payload == "" {
		t.Fatal("judged datagram receiver got no datagram from the relay")
	}
	info := stack.getJob(t, job.ID)
	if len(info.PushConnections) != 1 {
		t.Fatalf("push_connections = %+v, want one stream element", info.PushConnections)
	}
	got := info.PushConnections[0]
	want := protocol.PushConnection{
		Family: "udp", Purpose: "stream",
		Target: net.JoinHostPort(addr.String(), strconv.Itoa(port)),
		Origin: "request", Result: "sent",
	}
	if got != want {
		t.Fatalf("push_connections[0] = %+v, want %+v", got, want)
	}
}

func TestPushFamilyOutsideTableFailsInfraWithEmptyList(t *testing.T) {
	addr := privateIPv4(t)
	listener, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatalf("listen on judged address: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	// rtmpts has no row in C-141 (only rtmpt does), so it is a family outside
	// the table.
	target := "rtmpts://" + net.JoinHostPort(addr.String(), strconv.Itoa(port)) + "/live/stream"

	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conn.Close()
		accepted <- struct{}{}
	}()

	stack := newPushE2EStack(t)
	marker := filepath.Join(t.TempDir(), "ffmpeg-ran")
	ffmpegPath := filepath.Join(t.TempDir(), "ffmpeg-marker")
	if err := writeShellFixture(ffmpegPath, "touch "+shellSingleQuote(marker)+"\nexit 0\n"); err != nil {
		t.Fatalf("write marker ffmpeg: %v", err)
	}
	w := newFailureMatrixWorker(t, ffmpegPath, stack.server.URL, t.TempDir())
	job := testPushJob(t, stack, "push-outside", target, testUploadID("push-outside"))

	runJob(t, w, job)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ffmpeg ran for a target whose protocol family is outside C-141")
	}
	select {
	case <-accepted:
		t.Fatal("the judged address received a connection for an outside-table family")
	case <-time.After(300 * time.Millisecond):
	}
	info := stack.getJob(t, job.ID)
	if info.Status != protocol.JobStatusFailed {
		t.Fatalf("status = %s, want failed", info.Status)
	}
	if info.FailureType != string(protocol.FailureInfra) {
		t.Fatalf("failure_type = %q, want INFRA", info.FailureType)
	}
	if len(info.PushConnections) != 0 {
		t.Fatalf("push_connections = %+v, want an empty list", info.PushConnections)
	}
}
