package ratelimit

import (
	"testing"
)

func TestInMemoryCounter_TryIncrement(t *testing.T) {
	c := NewInMemoryCounter()
	clientID := "test-client"
	limit := 3

	// First 3 increments should succeed
	for i := 0; i < limit; i++ {
		current, allowed := c.TryIncrement(clientID, limit)
		if !allowed {
			t.Errorf("increment %d should be allowed, got current=%d", i+1, current)
		}
		if current != i+1 {
			t.Errorf("expected count %d, got %d", i+1, current)
		}
	}

	// 4th should be rejected
	current, allowed := c.TryIncrement(clientID, limit)
	if allowed {
		t.Error("4th increment should be rejected")
	}
	if current != limit {
		t.Errorf("expected count %d, got %d", limit, current)
	}

	// Verify rejected count
	if c.RejectedCount() != 1 {
		t.Errorf("expected 1 rejected, got %d", c.RejectedCount())
	}
}

func TestInMemoryCounter_Decrement(t *testing.T) {
	c := NewInMemoryCounter()
	clientID := "test-client"

	c.TryIncrement(clientID, 10)
	c.TryIncrement(clientID, 10)

	if c.Count(clientID) != 2 {
		t.Errorf("expected count 2, got %d", c.Count(clientID))
	}

	c.Decrement(clientID)
	if c.Count(clientID) != 1 {
		t.Errorf("expected count 1, got %d", c.Count(clientID))
	}

	c.Decrement(clientID)
	if c.Count(clientID) != 0 {
		t.Errorf("expected count 0, got %d", c.Count(clientID))
	}
}

func TestInMemoryCounter_RegisterJobAndDecrementByJob(t *testing.T) {
	c := NewInMemoryCounter()
	clientID := "test-client"
	jobID := "job-123"

	c.TryIncrement(clientID, 10)
	c.RegisterJob(clientID, jobID)

	if c.Count(clientID) != 1 {
		t.Errorf("expected count 1, got %d", c.Count(clientID))
	}

	c.DecrementByJob(jobID)
	if c.Count(clientID) != 0 {
		t.Errorf("expected count 0, got %d", c.Count(clientID))
	}
}

func TestInMemoryCounter_TotalActive(t *testing.T) {
	c := NewInMemoryCounter()

	c.TryIncrement("client-a", 10)
	c.TryIncrement("client-a", 10)
	c.TryIncrement("client-b", 10)

	if c.TotalActive() != 3 {
		t.Errorf("expected total 3, got %d", c.TotalActive())
	}
}

func TestInMemoryCounter_Concurrency(t *testing.T) {
	c := NewInMemoryCounter()
	done := make(chan bool)

	// Run 100 goroutines incrementing different clients
	for i := 0; i < 100; i++ {
		go func(id int) {
			clientID := "client"
			c.TryIncrement(clientID, 1000)
			c.Decrement(clientID)
			done <- true
		}(i)
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	if c.Count("client") != 0 {
		t.Errorf("expected count 0, got %d", c.Count("client"))
	}
}
