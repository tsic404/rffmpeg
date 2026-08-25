package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/server/db"
)

func newTestScheduler(t *testing.T) (*Scheduler, func()) {
	t.Helper()
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() failed: %v", err)
	}
	s := New(database, DefaultConfig())
	return s, func() { database.Close() }
}

// TestStopIdempotent verifies double-Stop does not panic (TSI-2365).
func TestStopIdempotent(t *testing.T) {
	s, cleanup := newTestScheduler(t)
	defer cleanup()

	s.Start()
	s.Stop()
	s.Stop() // must be a no-op, not a panic on double close
}

// TestStopConcurrent verifies concurrent Stop calls are safe (TSI-2365).
func TestStopConcurrent(t *testing.T) {
	s, cleanup := newTestScheduler(t)
	defer cleanup()

	s.Start()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Stop()
		}()
	}
	wg.Wait()
}

// TestStopBeforeStart verifies Stop without Start neither panics nor blocks (TSI-2365).
func TestStopBeforeStart(t *testing.T) {
	s, cleanup := newTestScheduler(t)
	defer cleanup()

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop before Start blocked forever")
	}
}
