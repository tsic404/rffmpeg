package worker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestUpdateJobWithFailure_Conflict verifies that a 409 response from the
// server surfaces as ErrJobConflict so callers can suppress benign log noise
// from a terminal-report race (e.g. CLI cancel racing the worker's timeout
// report — TSI-2451).
func TestUpdateJobWithFailure_Conflict(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"code":"conflict","message":"Job already in terminal state"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")
	err := client.UpdateJobWithFailure("job-race", protocol.JobStatusTimeout, 1, "timeout", false, "FFMPEG_ERROR", "details")
	if err == nil {
		t.Fatal("expected error for 409, got nil")
	}
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict, got: %v", err)
	}
}

// TestUpdateJobWithFailure_NonConflictError verifies that non-409 failure
// statuses do NOT report as conflict — a 500 is a real server fault.
func TestUpdateJobWithFailure_NonConflictError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":"internal_error","message":"db down"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")
	err := client.UpdateJobWithFailure("job-fail", protocol.JobStatusFailed, 1, "boom", false, "", "")
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if IsConflict(err) {
		t.Fatalf("500 must not report as conflict, got: %v", err)
	}
}

// TestUpdateJobWithFailure_Success verifies that a 200 response yields nil.
func TestUpdateJobWithFailure_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")
	err := client.UpdateJobWithFailure("job-ok", protocol.JobStatusCompleted, 0, "", true, "", "")
	if err != nil {
		t.Fatalf("expected nil error for 200, got: %v", err)
	}
}

// TestUpdateJobWithFailure_ConflictCarriesStatus verifies the wrapped error
// still mentions the HTTP status for diagnostics.
func TestUpdateJobWithFailure_ConflictCarriesStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"code":"conflict","message":"Job no longer assigned to this worker"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")
	err := client.UpdateJobWithFailure("job-wrap", protocol.JobStatusCancelled, -1, "cancelled", false, "", "")
	if err == nil {
		t.Fatal("expected error for 409, got nil")
	}
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict, got: %v", err)
	}
	// The underlying error string should mention 409 so operators still see
	// the original HTTP status in the debug-level log line.
	if !strings.Contains(err.Error(), "409") {
		t.Fatalf("expected error to mention status 409, got: %v", err)
	}
}

// TestIsConflict_NilError verifies IsConflict returns false for nil.
func TestIsConflict_NilError(t *testing.T) {
	if IsConflict(nil) {
		t.Fatal("IsConflict(nil) must be false")
	}
}
