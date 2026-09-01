package worker

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestBatcherTimerConcurrentAccess is the TSI-2705 regression test: the
// StderrBatcher / StdoutBatcher flushTimer field used to be written by the
// constructor and read by Close without holding b.mu, while the timer callback
// (timedFlush) re-arms the same timer under b.mu. time.AfterFunc invokes its
// callback in a new goroutine, so that unsynchronized write/read pair is a real
// data race caught by -race. A tiny BatchDelay makes the callback fire almost
// immediately, and concurrent New/Close stress reproduces the overlap.
func TestBatcherTimerConcurrentAccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "race-worker", "")

	stderrCfg := StderrBatcherConfig{BatchSize: 1, BatchDelay: time.Nanosecond}
	stdoutCfg := StdoutBatcherConfig{BatchSize: 1, BatchDelay: time.Nanosecond}

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b := NewStderrBatcher("race-stderr", client, stderrCfg)
			b.Close()
		}()
		go func() {
			defer wg.Done()
			b := NewStdoutBatcher("race-stdout", client, stdoutCfg)
			b.Close()
		}()
	}
	wg.Wait()
}
