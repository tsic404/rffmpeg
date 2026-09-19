package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestPullJobs_Conflict verifies a 409 pull surfaces as ErrPullConflict so the
// poll loop can distinguish "offline/evicted" from a real server fault.
func TestPullJobs_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"conflict","message":"Worker is offline"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "worker-1", "")
	_, err := c.PullJobs()
	if err == nil {
		t.Fatal("expected error for 409 pull, got nil")
	}
	if !IsPullConflict(err) {
		t.Fatalf("expected IsPullConflict, got: %v", err)
	}
}

// TestPullJobs_Evicted verifies a 409 pull carrying the worker_evicted code
// surfaces as ErrWorkerEvicted (and still satisfies IsPullConflict), so the
// poll loop keeps the eviction backoff distinct from offline recovery.
func TestPullJobs_Evicted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"worker_evicted","message":"Worker is evicted from scheduling"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "worker-1", "")
	_, err := c.PullJobs()
	if err == nil {
		t.Fatal("expected error for evicted 409 pull, got nil")
	}
	if !IsWorkerEvicted(err) {
		t.Fatalf("expected IsWorkerEvicted, got: %v", err)
	}
	if !IsPullConflict(err) {
		t.Fatalf("expected IsPullConflict for evicted 409, got: %v", err)
	}
}

// TestPullJobs_UnknownConflictCode verifies a 409 pull carrying an
// unrecognized code is treated conservatively as evicted (backoff), never as
// offline (immediate re-registration): noise or a future code must not clear
// an eviction flag.
func TestPullJobs_UnknownConflictCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"future_code","message":"something new"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "worker-1", "")
	_, err := c.PullJobs()
	if err == nil {
		t.Fatal("expected error for unknown-code 409 pull, got nil")
	}
	if !IsWorkerEvicted(err) {
		t.Fatalf("expected IsWorkerEvicted for unknown code, got: %v", err)
	}
}

// TestPullJobs_UndecodableConflictBody verifies a 409 pull whose body is not
// JSON is treated conservatively as evicted (backoff) rather than offline.
func TestPullJobs_UndecodableConflictBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`<html>bad gateway</html>`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "worker-1", "")
	_, err := c.PullJobs()
	if err == nil {
		t.Fatal("expected error for undecodable 409 pull, got nil")
	}
	if !IsWorkerEvicted(err) {
		t.Fatalf("expected IsWorkerEvicted for undecodable body, got: %v", err)
	}
}

// TestPullJobs_NonConflictError verifies a 500 pull does NOT report as a
// conflict — it is a real server fault that still triggers re-registration.
func TestPullJobs_NonConflictError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "worker-1", "")
	_, err := c.PullJobs()
	if err == nil {
		t.Fatal("expected error for 500 pull, got nil")
	}
	if IsPullConflict(err) {
		t.Fatalf("500 must not report as conflict, got: %v", err)
	}
}

// TestPullConflictBackoff pins the exponential schedule: start at 1s, double
// per consecutive conflict, cap at 30s. Inputs 1..6 are the values production
// reaches before the threshold re-registers (n<1 is the documented degradation
// to the initial delay).
func TestPullConflictBackoff(t *testing.T) {
	cases := []struct {
		consecutive int
		want        time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // capped
		{0, 1 * time.Second},  // n<1 degrades to the initial delay
	}
	for _, tc := range cases {
		if got := pullConflictBackoff(tc.consecutive); got != tc.want {
			t.Errorf("pullConflictBackoff(%d) = %v, want %v", tc.consecutive, got, tc.want)
		}
	}
}

// TestPollAndProcess_OfflineConflictReregistersImmediately verifies that an
// offline (server-restart) 409 pull conflict re-registers on the very first
// conflict instead of backing off: the worker must re-enter the pool within
// one poll tick rather than waiting out the 30s heartbeat clock.
func TestPollAndProcess_OfflineConflictReregistersImmediately(t *testing.T) {
	var mu sync.Mutex
	registrations := 0
	pulls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workers/register":
			mu.Lock()
			registrations++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"worker_id":"worker-1"}`))
		case "/api/v1/workers/worker-1/jobs":
			mu.Lock()
			pulls++
			mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"conflict","message":"Worker is offline; re-register to resume pulling"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	w, err := New(Config{ServerURL: srv.URL, WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	w.pollAndProcess(context.Background())

	mu.Lock()
	reg := registrations
	pullCount := pulls
	mu.Unlock()
	if reg != 1 {
		t.Fatalf("re-registered %d times on first offline conflict; want 1", reg)
	}
	if pullCount != 1 {
		t.Fatalf("made %d pulls; want 1", pullCount)
	}
}

// TestPollAndProcess_OfflineConflictFailedReregisterBacksOff verifies that an
// offline conflict whose immediate re-registration fails falls back to the
// eviction backoff (the sleep engages) instead of hammering the register
// endpoint once per poll tick.
func TestPollAndProcess_OfflineConflictFailedReregisterBacksOff(t *testing.T) {
	var mu sync.Mutex
	registrations := 0
	pulls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workers/register":
			mu.Lock()
			registrations++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/v1/workers/worker-1/jobs":
			mu.Lock()
			pulls++
			mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"conflict","message":"Worker is offline; re-register to resume pulling"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	slept := 0
	orig := pullConflictSleep
	pullConflictSleep = func(ctx context.Context, w *Worker, d time.Duration) { slept++ }
	defer func() { pullConflictSleep = orig }()

	w, err := New(Config{ServerURL: srv.URL, WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	w.pollAndProcess(context.Background())

	mu.Lock()
	reg := registrations
	pullCount := pulls
	mu.Unlock()
	if reg != 1 {
		t.Fatalf("re-registered %d times on first offline conflict; want exactly 1 immediate attempt", reg)
	}
	if pullCount != 1 {
		t.Fatalf("made %d pulls; want 1", pullCount)
	}
	if slept != 1 {
		t.Fatalf("backoff sleep engaged %d times after failed re-registration; want 1", slept)
	}
}

// TestPollAndProcess_EvictedConflictBacksOffThenReregisters verifies that an
// evicted (slow-node) 409 pull conflict backs off instead of re-registering
// immediately, and only re-registers once the conflict persists past the
// threshold — re-registering on eviction would clear the flag and flap a slow
// node back into scheduling.
func TestPollAndProcess_EvictedConflictBacksOffThenReregisters(t *testing.T) {
	var mu sync.Mutex
	registrations := 0
	pulls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workers/register":
			mu.Lock()
			registrations++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"worker_id":"worker-1"}`))
		case "/api/v1/workers/worker-1/jobs":
			mu.Lock()
			pulls++
			mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"worker_evicted","message":"Worker is evicted from scheduling"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Stub the backoff wait so the test runs instantly.
	orig := pullConflictSleep
	pullConflictSleep = func(ctx context.Context, w *Worker, d time.Duration) {}
	defer func() { pullConflictSleep = orig }()

	w, err := New(Config{ServerURL: srv.URL, WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// (threshold-1) consecutive conflicts stay below the threshold: back off,
	// no re-registration.
	for range pullConflictReregisterThreshold - 1 {
		w.pollAndProcess(context.Background())
	}

	mu.Lock()
	reg := registrations
	mu.Unlock()
	if reg != 0 {
		t.Fatalf("re-registered after %d consecutive conflicts; want 0 before threshold", pullConflictReregisterThreshold-1)
	}

	// The threshold-th consecutive conflict re-registers once.
	w.pollAndProcess(context.Background())

	mu.Lock()
	reg = registrations
	pullCount := pulls
	mu.Unlock()
	if reg != 1 {
		t.Fatalf("re-registered %d times after crossing threshold; want 1", reg)
	}
	if pullCount != pullConflictReregisterThreshold {
		t.Fatalf("made %d pulls; want %d", pullCount, pullConflictReregisterThreshold)
	}
}

// TestPollAndProcess_EvictedConflictResetsOnSuccess verifies a successful pull
// resets the eviction conflict counter. It drives the counter to (threshold-1)
// with consecutive evicted 409s, succeeds once, then issues one more — the
// "409 → success → 409" shape of sparse slow-node conflicts. That post-success
// 409 must restart at the initial tier, not cross the threshold: without the
// reset, it would be the threshold-th conflict and re-register.
func TestPollAndProcess_EvictedConflictResetsOnSuccess(t *testing.T) {
	var mu sync.Mutex
	registrations := 0
	pulls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workers/register":
			mu.Lock()
			registrations++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"worker_id":"worker-1"}`))
		case "/api/v1/workers/worker-1/jobs":
			mu.Lock()
			i := pulls
			pulls++
			mu.Unlock()
			// One successful pull at index threshold-1, 409 everywhere else.
			if i == pullConflictReregisterThreshold-1 {
				_, _ = w.Write([]byte(`{"jobs":[]}`))
				return
			}
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"worker_evicted","message":"Worker is evicted from scheduling"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	orig := pullConflictSleep
	pullConflictSleep = func(ctx context.Context, w *Worker, d time.Duration) {}
	defer func() { pullConflictSleep = orig }()

	w, err := New(Config{ServerURL: srv.URL, WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// (threshold-1) consecutive 409s fill the counter just below the threshold,
	// one successful pull resets it, then one more 409 starts over at 1.
	for range pullConflictReregisterThreshold - 1 {
		w.pollAndProcess(context.Background())
	}
	w.pollAndProcess(context.Background()) // success → reset
	w.pollAndProcess(context.Background()) // 409 → count restarts at 1

	mu.Lock()
	reg := registrations
	pullCount := pulls
	mu.Unlock()
	if reg != 0 {
		t.Fatalf("re-registered %d times after a 409→success→409 sequence; want 0 (success must reset the count)", reg)
	}
	wantPulls := pullConflictReregisterThreshold + 1
	if pullCount != wantPulls {
		t.Fatalf("made %d pulls; want %d", pullCount, wantPulls)
	}
}

// TestSendHeartbeat_EvictedSkipsReregistration verifies that a heartbeat
// rejected with worker_evicted does not trigger re-registration: re-registering
// clears the eviction flag and flaps a slow node back into scheduling, so the
// heartbeat path must skip it (matching the pull path's backoff).
func TestSendHeartbeat_EvictedSkipsReregistration(t *testing.T) {
	var mu sync.Mutex
	registrations := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workers/register":
			mu.Lock()
			registrations++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"worker_id":"worker-1"}`))
		case "/api/v1/workers/heartbeat":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"worker_evicted","message":"Worker is evicted from scheduling"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	w, err := New(Config{ServerURL: srv.URL, WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	w.sendHeartbeat()

	mu.Lock()
	reg := registrations
	mu.Unlock()
	if reg != 0 {
		t.Fatalf("re-registered %d times on evicted heartbeat; want 0", reg)
	}
}
