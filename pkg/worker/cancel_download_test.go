package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestProcessJob_CancelDuringInputDownload pins the pre-execution boundary: a
// cancellation that lands while the input is still downloading must abort the
// transfer and stop the job before ffmpeg starts, and must report the terminal
// cancelled status. Without a context on DownloadInput the worker kept
// downloading a job the client had already cancelled.
func TestProcessJob_CancelDuringInputDownload(t *testing.T) {
	w, _ := setupTestWorker(t, time.Hour)

	// Marker written only if the executor is reached; the stock mock ffmpeg
	// writes into the job dir, which is removed by the deferred cleanup.
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "ffmpeg-ran")
	script := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(script, []byte("#!/bin/bash\ntouch '"+marker+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write mock ffmpeg: %v", err)
	}
	w.executor = NewExecutor(script, 30*time.Second)

	downloadStarted := make(chan struct{})
	downloadAborted := make(chan struct{})
	var mu sync.Mutex
	var patches []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/files/"):
			close(downloadStarted)
			<-r.Context().Done()
			close(downloadAborted)
		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			patches = append(patches, string(body))
			mu.Unlock()
			_, _ = rw.Write([]byte(`{"message":"ok"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = rw.Write([]byte(`{"message":"ok","cancelled_jobs":[]}`))
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	w.client.baseURL = srv.URL

	jobCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := protocol.JobInfo{
		ID:         "cancel-during-download",
		InputFiles: []string{"file-1"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}

	done := make(chan struct{})
	go func() {
		w.processJob(jobCtx, job, cancel, false)
		close(done)
	}()

	select {
	case <-downloadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("input download never started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("processJob did not return after cancellation during the input download")
	}
	select {
	case <-downloadAborted:
	default:
		t.Fatal("the in-flight input download was not aborted by the cancellation")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ffmpeg ran after the job was cancelled during the input download")
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(patches, "\n"), `"status":"cancelled"`) {
		t.Fatalf("no cancelled terminal report; patches=%v", patches)
	}
}
