package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// jobStatusServer serves the job status endpoint from poll, counting hits.
func jobStatusServer(t *testing.T, poll func(hits int64) (int, protocol.JobInfo)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, job := poll(hits.Add(1))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{Job: job})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestWaitForJob_TransientPollErrorKeepsWaiting pins the HTTP fallback: one
// answered error is not a verdict. The wait must keep polling and still hand
// back the terminal job, so a network hiccup never turns a running job into a
// client-side failure.
func TestWaitForJob_TransientPollErrorKeepsWaiting(t *testing.T) {
	oldPoll := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = oldPoll }()

	srv, hits := jobStatusServer(t, func(hit int64) (int, protocol.JobInfo) {
		if hit == 1 {
			return http.StatusInternalServerError, protocol.JobInfo{}
		}
		return http.StatusOK, protocol.JobInfo{ID: "job-1", Status: protocol.JobStatusCompleted}
	})

	c := New(srv.URL, "", WithMaxRetries(DefaultMaxRetries))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	job, err := c.WaitForJob(ctx, "job-1", false)
	if err != nil {
		t.Fatalf("WaitForJob returned error after a single failed poll: %v", err)
	}
	if job.Status != protocol.JobStatusCompleted {
		t.Fatalf("status = %s, want completed", job.Status)
	}
	if got := hits.Load(); got < 2 {
		t.Fatalf("status endpoint hit %d time(s), want the failed poll retried", got)
	}
}

// TestWaitForJob_ExhaustedBudgetReportsRetriesExhausted pins the give-up
// verdict: when every poll fails for the whole retry budget, the wait must
// report RetriesExhaustedError (the CLI maps it to the distinct "submitted but
// abandoned" exit code) instead of an ordinary error.
func TestWaitForJob_ExhaustedBudgetReportsRetriesExhausted(t *testing.T) {
	oldPoll := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = oldPoll }()

	srv, _ := jobStatusServer(t, func(int64) (int, protocol.JobInfo) {
		return http.StatusInternalServerError, protocol.JobInfo{}
	})

	c := New(srv.URL, "", WithMaxRetries(1))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.WaitForJob(ctx, "job-1", false)
	var re *RetriesExhaustedError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v (%T), want *RetriesExhaustedError", err, err)
	}
	if re.JobID != "job-1" {
		t.Errorf("RetriesExhaustedError.JobID = %q, want job-1", re.JobID)
	}
}

// TestWaitForJob_JobNotFoundIsNotRetried pins the other side: a 404 is the
// server's answer that the job does not exist, so it is reported at once
// rather than retried for the whole budget.
func TestWaitForJob_JobNotFoundIsNotRetried(t *testing.T) {
	oldPoll := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = oldPoll }()

	srv, hits := jobStatusServer(t, func(int64) (int, protocol.JobInfo) {
		return http.StatusNotFound, protocol.JobInfo{}
	})

	c := New(srv.URL, "", WithMaxRetries(DefaultMaxRetries))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.WaitForJob(ctx, "job-1", false)
	if !errors.Is(err, protocol.ErrJobNotFound) {
		t.Fatalf("error = %v, want protocol.ErrJobNotFound", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("status endpoint hit %d time(s), want the 404 verdict accepted on the first answer", got)
	}
}

// TestWaitForJobWithLogs_NoPollingAfterReturn pins the helper lifetime: once
// the wait returns, neither the backup poll nor the WS reader may keep
// running. The status endpoint counts its hits, so a helper still alive past
// the return shows up as a growing count.
func TestWaitForJobWithLogs_NoPollingAfterReturn(t *testing.T) {
	oldPoll := pollInterval
	pollInterval = 5 * time.Millisecond
	defer func() { pollInterval = oldPoll }()

	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		msg, err := json.Marshal(protocol.NewStatusMessage("job-1", protocol.JobStatusCompleted, 0, ""))
		if err != nil {
			t.Errorf("marshal status message: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{
			Job: protocol.JobInfo{ID: "job-1", Status: protocol.JobStatusCompleted},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "", WithMaxRetries(DefaultMaxRetries))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	job, err := c.WaitForJobWithLogs(ctx, "job-1", true)
	if err != nil {
		t.Fatalf("WaitForJobWithLogs: %v", err)
	}
	if job.Status != protocol.JobStatusCompleted {
		t.Fatalf("status = %s, want completed", job.Status)
	}

	settled := hits.Load()
	time.Sleep(20 * pollInterval)
	if got := hits.Load(); got != settled {
		t.Fatalf("status endpoint hit %d time(s) after the wait returned (was %d): the backup poll outlives WaitForJobWithLogs", got, settled)
	}
}
