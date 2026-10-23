package handlers_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/cli/client"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// These tests pin the user-visible copy of the submission failure paths. The
// acceptance criteria judge stderr lines verbatim, so a reworded errMsg must
// fail here — asserting only status codes and failure types cannot catch it.

const (
	// wantEncoderUnavailableMsg is AC-13's message, printed by the CLI verbatim
	// after the "Job failed: [ENCODER_UNAVAILABLE] " tag.
	wantEncoderUnavailableMsg = "No worker available with encoder: libx265 (or compatible encoders)"
	// wantWorkerCrashMsg is AC-17's message, printed by the CLI verbatim after
	// the "Job failed: [WORKER_CRASH] " tag.
	wantWorkerCrashMsg = "worker crashed (heartbeat timeout) and no surviving worker to take over the job"
	// wantNoWorkerLine is AC-16's full stderr line.
	wantNoWorkerLine = "Error submitting job: job submission failed [worker_unavailable]: No workers available. Please ensure at least one worker is registered and online."
)

// cliSubmitFailureLine renders a submission error exactly as the CLI writes it
// to stderr: "Error submitting job: <error>".
func cliSubmitFailureLine(err error) string {
	return "Error submitting job: " + err.Error()
}

// getJobForCopyTest reads a job through the public status endpoint.
func getJobForCopyTest(t *testing.T, router http.Handler, jobID string) protocol.JobInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get job %s: status %d, body %s", jobID, w.Code, w.Body.String())
	}
	var resp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	return resp.Job
}

func listJobsForCopyTest(t *testing.T, router http.Handler) []protocol.JobInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list jobs: status %d, body %s", w.Code, w.Body.String())
	}
	var resp protocol.JobListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode job list response: %v", err)
	}
	return resp.Jobs
}

// TestSubmitFailureCopy_EncoderUnavailable pins AC-13: the submit response and
// the persisted terminal job both carry the verbatim user-visible message.
func TestSubmitFailureCopy_EncoderUnavailable(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	registerTestWorker(t, router, []string{"libx264"})
	fileID := uploadTestFile(t, router)

	jobBody, err := json.Marshal(protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "libx265", "-preset", "fast"},
	})
	if err != nil {
		t.Fatalf("marshal job request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (job persisted as failed): %s", w.Code, w.Body.String())
	}

	var submitResp protocol.JobSubmitResponse
	if err := json.NewDecoder(w.Body).Decode(&submitResp); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	if submitResp.Message != wantEncoderUnavailableMsg {
		t.Errorf("submit response message = %q, want %q", submitResp.Message, wantEncoderUnavailableMsg)
	}

	job := getJobForCopyTest(t, router, submitResp.JobID)
	if job.Status != protocol.JobStatusFailed {
		t.Errorf("job status = %q, want %q", job.Status, protocol.JobStatusFailed)
	}
	if job.FailureType != string(protocol.FailureEncoderUnavailable) {
		t.Errorf("failure_type = %q, want %q", job.FailureType, protocol.FailureEncoderUnavailable)
	}
	if job.Error != wantEncoderUnavailableMsg {
		t.Errorf("job error (printed after \"Job failed: [ENCODER_UNAVAILABLE] \") = %q, want %q",
			job.Error, wantEncoderUnavailableMsg)
	}
}

// TestSubmitFailureCopy_NoWorkerAvailable pins AC-16: fail-fast submission with
// no live worker produces the exact stderr line and persists no job.
func TestSubmitFailureCopy_NoWorkerAvailable(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	fileID := uploadTestFile(t, router)

	srv := httptest.NewServer(router)
	defer srv.Close()
	c := client.New(srv.URL, "test-token")

	jobID, err := c.SubmitJob([]string{fileID}, []string{"-c:v", "libx264", "-preset", "fast"}, "out.mp4", false)
	if err == nil {
		t.Fatalf("submission with no live worker succeeded (job %q), want fail-fast rejection", jobID)
	}
	if jobID != "" {
		t.Errorf("job ID = %q, want empty on rejected submission", jobID)
	}
	if got := cliSubmitFailureLine(err); got != wantNoWorkerLine {
		t.Errorf("stderr line = %q, want %q", got, wantNoWorkerLine)
	}

	if jobs := listJobsForCopyTest(t, router); len(jobs) != 0 {
		t.Errorf("jobs created = %d, want 0 (worker_unavailable must not persist a job)", len(jobs))
	}
}

// TestSubmitFailureCopy_RateLimitExceeded pins AC-29: the rejection carries the
// live concurrent count, so the expected line is matched and N captured rather
// than hard-coded.
func TestSubmitFailureCopy_RateLimitExceeded(t *testing.T) {
	const limit = 10
	_, router, cleanup := setupTestWithRateLimit(t, limit)
	defer cleanup()

	registerWorkerWithAuth(t, router, "worker-1", []string{"libx264"})
	fileID := uploadTestFile(t, router)

	srv := httptest.NewServer(router)
	defer srv.Close()
	c := client.New(srv.URL, "test-token")

	args := []string{"-c:v", "libx264", "-preset", "fast"}
	for i := range limit {
		if _, err := c.SubmitJob([]string{fileID}, args, "out.mp4", false); err != nil {
			t.Fatalf("submission %d/%d within the limit failed: %v", i+1, limit, err)
		}
	}

	_, err := c.SubmitJob([]string{fileID}, args, "out.mp4", false)
	if err == nil {
		t.Fatalf("submission %d exceeded the limit but succeeded", limit+1)
	}

	line := cliSubmitFailureLine(err)
	pattern := regexp.MustCompile(fmt.Sprintf(
		`^Error submitting job: job submission failed \[rate_limit_exceeded\]: rate limit exceeded: (\d+)/%d concurrent jobs$`, limit))
	m := pattern.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("stderr line = %q, want \"Error submitting job: job submission failed [rate_limit_exceeded]: rate limit exceeded: N/%d concurrent jobs\"", line, limit)
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		t.Fatalf("parse live concurrent count %q: %v", m[1], convErr)
	}
	if n < limit {
		t.Errorf("live concurrent count = %d, want >= %d (every slot held)", n, limit)
	}
}
