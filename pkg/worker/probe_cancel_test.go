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

// TestProcessProbeJob_CancelDuringInputDownload pins the probe path's
// cancellation semantics: a probe whose input download is cancelled must
// report the terminal cancelled status (not failed as INFRA) and must not run
// ffprobe. The fake ffprobe on PATH writes a marker only if it is executed.
func TestProcessProbeJob_CancelDuringInputDownload(t *testing.T) {
	w, _ := setupTestWorker(t, time.Hour)

	tmp := t.TempDir()
	marker := filepath.Join(tmp, "ffprobe-ran")
	fakeProbe := filepath.Join(tmp, "ffprobe")
	if err := os.WriteFile(fakeProbe, []byte("#!/bin/bash\ntouch '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	// processProbeJob resolves ffprobe from PATH at first use.
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))

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
		ID:         "cancel-probe",
		InputFiles: []string{"file-1"},
		Args:       []string{"__rffmpeg_probe__", "file-1"},
	}

	done := make(chan struct{})
	go func() {
		w.processJob(jobCtx, job, cancel, false)
		close(done)
	}()

	select {
	case <-downloadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("probe input download never started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("processJob did not return after cancelling the probe input download")
	}
	select {
	case <-downloadAborted:
	default:
		t.Fatal("the in-flight probe input download was not aborted by the cancellation")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ffprobe ran after the probe job was cancelled")
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(patches, "\n")
	if !strings.Contains(joined, `"status":"cancelled"`) {
		t.Fatalf("cancelled probe did not report the cancelled terminal status; patches=%v", patches)
	}
	if strings.Contains(joined, `"failure_type":"INFRA"`) {
		t.Fatalf("cancelled probe reported an INFRA failure; patches=%v", patches)
	}
}
