package accesslog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writerFunc adapts a function to io.Writer.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestMiddlewareKeepsResponsesOffTheStalledSink pins the contract this package
// exists for: a log destination that stops accepting writes must not delay the
// responses it logs. chi's own middleware.Logger writes the line on the response
// path, so with it the first write into a stalled destination hangs the request
// that produced it and every request behind it — health probes included.
func TestMiddlewareKeepsResponsesOffTheStalledSink(t *testing.T) {
	release := make(chan struct{})
	destination := writerFunc(func(p []byte) (int, error) {
		<-release
		return len(p), nil
	})

	const queueDepth = 4
	const requests = 25
	sink := NewSink(destination, queueDepth)
	defer sink.Close()

	srv := httptest.NewServer(Middleware(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	for i := 1; i <= requests; i++ {
		start := time.Now()
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d waited on the stalled log destination: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("request %d took %v with a stalled log destination; responses must not wait for it", i, elapsed)
		}
	}

	if got := sink.Dropped(); got == 0 {
		t.Errorf("Dropped() = 0 after %d requests through a %d-line queue into a stalled destination, want > 0",
			requests, queueDepth)
	}
	close(release)
}

// TestSinkFlushesQueuedLinesAndReportsDroppedOnes pins the lossy-but-bounded
// contract: lines that fit the queue reach the destination in order, the rest
// are dropped, and the drop is reported once the destination accepts writes
// again.
func TestSinkFlushesQueuedLinesAndReportsDroppedOnes(t *testing.T) {
	var out bytes.Buffer
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	destination := writerFunc(func(p []byte) (int, error) {
		once.Do(func() { close(entered) })
		<-release
		return out.Write(p)
	})

	sink := NewSink(destination, 2)
	defer sink.Close()

	sink.Write([]byte("line-0\n"))
	<-entered // the drain goroutine is now parked inside the stalled destination
	for i := 1; i <= 6; i++ {
		sink.Write([]byte(fmt.Sprintf("line-%d\n", i)))
	}

	if got := sink.Dropped(); got != 4 {
		t.Fatalf("Dropped() = %d, want 4 (6 lines offered into a 2-line queue)", got)
	}
	close(release)
	sink.Close()

	got := out.String()
	for _, want := range []string{"line-0\n", "line-1\n", "line-2\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("queued line %q missing from destination output %q", want, got)
		}
	}
	for _, dropped := range []string{"line-4", "line-5", "line-6"} {
		if strings.Contains(got, dropped) {
			t.Errorf("dropped line %q reached the destination: %q", dropped, got)
		}
	}
	if !strings.Contains(got, "lost 4 access log line(s)") || !strings.Contains(got, "4 dropped from a full queue") {
		t.Errorf("destination output %q does not report the 4 dropped lines", got)
	}
}

// TestSinkCountsFailingWritesAndReportsThem pins the other loss path: a
// destination that accepts the write call but returns an error (stdout
// redirected to a full disk) must be counted and reported, not swallowed.
func TestSinkCountsFailingWritesAndReportsThem(t *testing.T) {
	var out bytes.Buffer
	failing := atomic.Bool{}
	failing.Store(true)
	var failed atomic.Int64
	twoFailed := make(chan struct{})
	destination := writerFunc(func(p []byte) (int, error) {
		if failing.Load() {
			if failed.Add(1) == 2 {
				close(twoFailed)
			}
			return 0, errors.New("destination write failed")
		}
		return out.Write(p)
	})

	sink := NewSink(destination, 8)
	defer sink.Close()

	sink.Write([]byte("line-0\n"))
	sink.Write([]byte("line-1\n"))
	<-twoFailed // both lines have been handed to the failing destination

	failing.Store(false)
	sink.Write([]byte("line-2\n"))
	sink.Close()

	got := out.String()
	if !strings.Contains(got, "lost 2 access log line(s)") || !strings.Contains(got, "2 write errors") {
		t.Errorf("destination output %q does not report the 2 failed writes", got)
	}
	if !strings.Contains(got, "line-2") {
		t.Errorf("destination output %q does not contain the line that followed the failures", got)
	}
}

// TestSinkCloseGivesUpOnStalledDestination pins that shutdown is bounded too: a
// destination that never accepts another write must not hold Close forever.
func TestSinkCloseGivesUpOnStalledDestination(t *testing.T) {
	original := closeDrainTimeout
	closeDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { closeDrainTimeout = original })

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sink := NewSink(writerFunc(func(p []byte) (int, error) {
		once.Do(func() { close(entered) })
		<-release
		return len(p), nil
	}), 1)

	sink.Write([]byte("line\n"))
	<-entered

	start := time.Now()
	sink.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close() took %v with a stalled destination, want it to give up after closeDrainTimeout", elapsed)
	}
	close(release)
}

// TestMiddlewareLogsRequests pins that the middleware still produces the access
// line — the response path is decoupled from the destination, not the log.
func TestMiddlewareLogsRequests(t *testing.T) {
	var out bytes.Buffer
	sink := NewSink(&out, 8)

	srv := httptest.NewServer(Middleware(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))
	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	srv.Close()
	sink.Close()

	line := out.String()
	if !strings.Contains(line, "GET") || !strings.Contains(line, "/api/v1/health") || !strings.Contains(line, "418") {
		t.Errorf("access log line %q does not describe the request", line)
	}
}
