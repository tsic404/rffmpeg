package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// submitRetryServer returns an httptest server that rate-limits (429) the
// first failCount POST /api/v1/jobs requests, then accepts subsequent ones.
// It records the number of submission attempts seen.
func submitRetryServer(t *testing.T, failCount int32) (*httptest.Server, *int32) {
	t.Helper()

	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= failCount {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(protocol.RateLimitResponse{
				Code:    protocol.ErrCodeRateLimitExceeded,
				Message: "Too many concurrent jobs.",
				Current: 10,
				Limit:   10,
				RetryIn: 1,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobSubmitResponse{JobID: "job-1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &attempts
}

// stubSubmitSleep makes the rate-limit backoff run instantly for the duration
// of a test, and returns a restore func.
func stubSubmitSleep(t *testing.T) func() {
	t.Helper()
	oldSleep := submitRetrySleep
	submitRetrySleep = func(context.Context, time.Duration) error { return nil }
	return func() { submitRetrySleep = oldSleep }
}

// TestSubmitJobWithOptions_RetriesRateLimit pins the happy path: with a
// submit-retry budget, a transient 429 is retried and the job eventually
// submits instead of failing immediately (TSI-2939).
func TestSubmitJobWithOptions_RetriesRateLimit(t *testing.T) {
	defer stubSubmitSleep(t)()

	srv, attempts := submitRetryServer(t, 2)
	c := New(srv.URL, "", WithSubmitRetries(3))

	jobID, err := c.SubmitJob(nil, nil, "out.mp4", false)
	if err != nil {
		t.Fatalf("SubmitJob returned error after retry: %v", err)
	}
	if jobID != "job-1" {
		t.Errorf("jobID = %q, want %q", jobID, "job-1")
	}
	if got := atomic.LoadInt32(attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 (2 rate-limited + 1 success)", got)
	}
}

// TestSubmitJobWithOptions_NoRetryByDefault pins the default: without
// WithSubmitRetries a 429 fails immediately with a single attempt.
func TestSubmitJobWithOptions_NoRetryByDefault(t *testing.T) {
	srv, attempts := submitRetryServer(t, 100)
	c := New(srv.URL, "")

	_, err := c.SubmitJob(nil, nil, "out.mp4", false)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error = %v (%T), want *RateLimitError", err, err)
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry by default)", got)
	}
}

// TestSubmitJobWithOptions_RetryExhausted pins the bound: a persistently
// rate-limited endpoint is tried exactly (budget+1) times, then the last
// rate-limit error is surfaced.
func TestSubmitJobWithOptions_RetryExhausted(t *testing.T) {
	defer stubSubmitSleep(t)()

	srv, attempts := submitRetryServer(t, 100)
	c := New(srv.URL, "", WithSubmitRetries(2))

	_, err := c.SubmitJob(nil, nil, "out.mp4", false)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error = %v (%T), want *RateLimitError", err, err)
	}
	if got := atomic.LoadInt32(attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 (1 initial + 2 retries)", got)
	}
}

// TestSubmitJobWithOptions_NonRateLimitNoRetry pins that only 429 is retried:
// a 500 fails immediately even with a retry budget.
func TestSubmitJobWithOptions_NonRateLimitNoRetry(t *testing.T) {
	defer stubSubmitSleep(t)()

	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "", WithSubmitRetries(3))
	_, err := c.SubmitJob(nil, nil, "out.mp4", false)
	if err == nil {
		t.Fatal("SubmitJob returned nil error, want failure")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (non-429 is not retried)", got)
	}
}

// TestSubmitJobWithOptions_CancelledCtxAbortsBackoff pins the signal-fix
// (TSI-2939): a ctx cancelled during the rate-limit backoff aborts the retry
// loop and returns ctx.Err() instead of sleeping through the full backoff.
func TestSubmitJobWithOptions_CancelledCtxAbortsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var slept time.Duration
	oldSleep := submitRetrySleep
	submitRetrySleep = func(c context.Context, d time.Duration) error {
		slept += d
		cancel() // simulate the signal handler interrupting the backoff
		return sleepWithContext(c, d)
	}
	defer func() { submitRetrySleep = oldSleep }()

	srv, attempts := submitRetryServer(t, 100)
	c := New(srv.URL, "", WithSubmitRetries(5))

	_, err := c.SubmitJobWithOptions(ctx, nil, nil, nil, "out.mp4", false, false, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (cancelled before the second attempt)", got)
	}
	if slept <= 0 {
		t.Errorf("slept = %v, want a positive backoff before cancellation", slept)
	}
}

// errReader is an io.Reader that always fails, to exercise decodeRateLimitError's
// io.ReadAll error path.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failure") }

// TestDecodeRateLimitError covers all three branches of the 429 decoder: the
// structured JSON path and the two fallback paths (undecodable body and read
// failure), which previously had no coverage (TSI-2939).
func TestDecodeRateLimitError(t *testing.T) {
	// Structured JSON populates the fields and renders the single-line message.
	err := decodeRateLimitError(strings.NewReader(
		`{"code":"rate_limit_exceeded","message":"too many","current":7,"limit":5,"retry_in":3}`))
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error = %v (%T), want *RateLimitError", err, err)
	}
	if !rl.decoded || rl.Current != 7 || rl.Limit != 5 || rl.RetryIn != 3 {
		t.Errorf("decoded fields = %+v, want decoded current=7 limit=5 retry_in=3", rl)
	}
	if rl.rawBody != "" {
		t.Errorf("rawBody = %q, want empty for a decoded JSON body", rl.rawBody)
	}
	if got, want := rl.Error(), "rate limit exceeded: 7/5 concurrent jobs. Retry after 3 seconds"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	// Undecodable body is preserved verbatim in the diagnostic.
	err = decodeRateLimitError(strings.NewReader("not json"))
	if !errors.As(err, &rl) {
		t.Fatalf("error = %v (%T), want *RateLimitError", err, err)
	}
	if rl.decoded {
		t.Errorf("decoded = true for a non-JSON body, want false")
	}
	if rl.rawBody != "not json" {
		t.Errorf("rawBody = %q, want %q", rl.rawBody, "not json")
	}
	if !strings.Contains(rl.Error(), "not json") {
		t.Errorf("Error() = %q, want it to preserve the raw body", rl.Error())
	}

	// A read failure yields an empty rate-limit error with the bare message.
	err = decodeRateLimitError(errReader{})
	if !errors.As(err, &rl) {
		t.Fatalf("error = %v (%T), want *RateLimitError", err, err)
	}
	if rl.decoded {
		t.Errorf("decoded = true for a read failure, want false")
	}
	if got, want := rl.Error(), "rate limit exceeded (HTTP 429)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestRateLimitBackoff pins the exponential schedule: start at retry_in,
// double per attempt, cap at submitRetryMaxDelay, default to 1s when the
// server gave no hint, and clamp an overflowing retry_in before the Duration
// conversion (TSI-2939).
func TestRateLimitBackoff(t *testing.T) {
	cases := []struct {
		name    string
		retryIn int
		attempt int
		want    time.Duration
	}{
		{"first attempt honors retry_in", 5, 0, 5 * time.Second},
		{"second attempt doubles", 5, 1, 10 * time.Second},
		{"fourth attempt doubles again", 5, 3, 40 * time.Second},
		{"missing hint defaults to 1s", 0, 0, time.Second},
		{"capped at submitRetryMaxDelay", 5, 10, submitRetryMaxDelay},
		{"retry_in above cap is clamped", int(submitRetryMaxDelay/time.Second) + 100, 0, submitRetryMaxDelay},
		{"overflowing retry_in is clamped, not wrapped", 1 << 40, 0, submitRetryMaxDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateLimitBackoff(tc.retryIn, tc.attempt); got != tc.want {
				t.Errorf("rateLimitBackoff(%d, %d) = %v, want %v", tc.retryIn, tc.attempt, got, tc.want)
			}
		})
	}
}
