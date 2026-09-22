package worker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestReportInputDownloadFailureFriendlyMessage locks the fix: a
// user-supplied remote URL whose fetch fails at the transport layer (a
// dataClient.Do error) is INPUT_UNREACHABLE, and its user-facing Error must be
// a clear message naming the input — not the raw net/http transport text
// ("dial tcp: lookup … : no such host") — while the raw text is preserved in
// failure_details for operators.
func TestReportInputDownloadFailureFriendlyMessage(t *testing.T) {
	var update protocol.JobUpdateRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	w := &Worker{client: NewClient(srv.URL, "worker-1", "")}

	downloadErr := &transportError{err: fmt.Errorf("dial tcp: lookup invalid.example: no such host")}

	w.reportInputDownloadFailure("job-remote", "http://invalid.example/video.mp4", downloadErr)

	if update.FailureType != string(protocol.FailureInputUnreachable) {
		t.Fatalf("FailureType = %q, want %q", update.FailureType, protocol.FailureInputUnreachable)
	}
	if want := "Input file or stream cannot be reached: http://invalid.example/video.mp4"; update.Error != want {
		t.Errorf("Error = %q, want %q", update.Error, want)
	}
	if !strings.Contains(update.FailureDetails, "dial tcp: lookup invalid.example: no such host") {
		t.Errorf("FailureDetails = %q, want raw transport error preserved", update.FailureDetails)
	}
	if strings.Contains(update.Error, "no such host") {
		t.Errorf("Error = %q, must not surface raw transport text", update.Error)
	}
}

// TestReportInputDownloadFailureServerFileKeepsRawMessage locks the INFRA side
// of the classification: a server file ID failing over the worker↔server
// channel is infrastructure, not user input, so the raw message stays in Error
// unchanged — the friendly INPUT_UNREACHABLE mapping must not apply.
func TestReportInputDownloadFailureServerFileKeepsRawMessage(t *testing.T) {
	var update protocol.JobUpdateRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	w := &Worker{client: NewClient(srv.URL, "worker-1", "")}

	downloadErr := fmt.Errorf("connection reset by peer")

	w.reportInputDownloadFailure("job-serverfile", "server-file-001", downloadErr)

	if update.FailureType != string(protocol.FailureInfra) {
		t.Fatalf("FailureType = %q, want %q", update.FailureType, protocol.FailureInfra)
	}
	if want := "Failed to download input file server-file-001: connection reset by peer"; update.Error != want {
		t.Errorf("Error = %q, want %q", update.Error, want)
	}
}

// TestReportInputDownloadFailureDiskFull locks the fix: a worker disk-full
// while writing a downloaded server-file input is reported as DISK_FULL, not
// INFRA — the user must see a local out-of-space condition, not a server
// channel fault — and the actionable error text passes through unchanged.
func TestReportInputDownloadFailureDiskFull(t *testing.T) {
	var update protocol.JobUpdateRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	w := &Worker{client: NewClient(srv.URL, "worker-1", "")}

	downloadErr := fmt.Errorf("failed to write file: %w", syscall.ENOSPC)

	w.reportInputDownloadFailure("job-diskfull", "server-file-001", downloadErr)

	if update.FailureType != string(protocol.FailureDiskFull) {
		t.Fatalf("FailureType = %q, want %q", update.FailureType, protocol.FailureDiskFull)
	}
	if !strings.Contains(update.Error, "no space left on device") {
		t.Errorf("Error = %q, want the out-of-space cause preserved", update.Error)
	}
	if strings.Contains(update.Error, "cannot be reached") {
		t.Errorf("Error = %q, must not apply the INPUT_UNREACHABLE friendly mapping", update.Error)
	}
}

// TestReportInputDownloadFailureOversizeKeepsOriginalMessage locks the
// boundary: a remote URL whose fetch succeeds but exceeds the size cap is not
// "unreachable", so the actionable size-limit message must stay in Error — the
// friendly INPUT_UNREACHABLE mapping applies only to transport-layer failures.
func TestReportInputDownloadFailureOversizeKeepsOriginalMessage(t *testing.T) {
	var update protocol.JobUpdateRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	w := &Worker{client: NewClient(srv.URL, "worker-1", "")}

	downloadErr := fmt.Errorf("remote input exceeds maximum size of %d bytes", MaxRemoteInputBytes)

	w.reportInputDownloadFailure("job-oversize", "http://valid.example/huge.mp4", downloadErr)

	if update.FailureType != string(protocol.FailureInputUnreachable) {
		t.Fatalf("FailureType = %q, want %q", update.FailureType, protocol.FailureInputUnreachable)
	}
	if !strings.Contains(update.Error, "remote input exceeds maximum size") {
		t.Errorf("Error = %q, want original size-limit message preserved", update.Error)
	}
	if strings.Contains(update.Error, "cannot be reached") {
		t.Errorf("Error = %q, must not apply friendly mapping to non-transport failure", update.Error)
	}
}
