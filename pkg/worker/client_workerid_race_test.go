package worker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestClientWorkerIDReadsAreSynchronized drives every request that embeds the
// worker identity while the identity is being swapped by re-registration. The
// race detector is the assertion: a bare field read here fails the -race gate.
func TestClientWorkerIDReadsAreSynchronized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "worker-0", "")

	const iterations = 100
	calls := map[string]func() error{
		"UpdateJob":       func() error { return client.UpdateJob("job-1", protocol.JobStatusCompleted, 0, "", false) },
		"SendStderrChunk": func() error { return client.SendStderrChunk("job-1", "chunk") },
		"SendStdoutChunk": func() error { return client.SendStdoutChunk("job-1", []byte("chunk")) },
		"SendProgress":    func() error { return client.SendProgress("job-1", 1, 1, 1, 1, 1) },
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range iterations {
			client.SetWorkerID(fmt.Sprintf("worker-%d", i))
		}
	}()

	for name, call := range calls {
		wg.Add(1)
		go func(name string, call func() error) {
			defer wg.Done()
			for range iterations {
				if err := call(); err != nil {
					t.Errorf("%s: %v", name, err)
					return
				}
			}
		}(name, call)
	}
	wg.Wait()
}
