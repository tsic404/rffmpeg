package ratelimit

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/auth"
)

// RuntimeConfig holds rate limit configuration that can be updated at runtime.
type RuntimeConfig struct {
	mu      sync.RWMutex
	Enabled bool `json:"enabled"`
	Limit   int  `json:"limit"`
}

// IsEnabled returns whether rate limiting is enabled (thread-safe).
func (c *RuntimeConfig) IsEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Enabled
}

// GetLimit returns the maximum concurrent jobs per client (thread-safe).
func (c *RuntimeConfig) GetLimit() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Limit
}

// SetEnabled updates the Enabled flag (thread-safe).
func (c *RuntimeConfig) SetEnabled(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Enabled = v
}

// SetLimit updates the Limit (thread-safe).
func (c *RuntimeConfig) SetLimit(v int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Limit = v
}

// DefaultRuntimeConfig returns the default runtime rate limit configuration.
func DefaultRuntimeConfig() *RuntimeConfig {
	return &RuntimeConfig{
		Enabled: true,
		Limit:   10,
	}
}

// JobSubmitMiddleware returns an HTTP middleware for job submission endpoints.
// It performs a pre-check: increments the client's active job count before
// the handler runs. If the limit is exceeded, it returns 429 immediately.
// If the handler fails (non-2xx), the increment is rolled back.
//
// The actual job-to-client mapping must be registered by the handler
// via counter.RegisterJob() after successful job creation.
func JobSubmitMiddleware(counter ClientJobCounter, runtimeCfg *RuntimeConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !runtimeCfg.IsEnabled() {
				next.ServeHTTP(w, r)
				return
			}

			clientID := auth.GetClientID(r)
			if clientID == "" {
				// Fall back to remote IP when no auth context (e.g., worker requests
				// or unauthenticated clients) so rate limiting still applies.
				clientID = r.RemoteAddr
			}

			limit := runtimeCfg.GetLimit()
			current, allowed := counter.TryIncrement(clientID, limit)
			if !allowed {
				writeRateLimitResponse(w, current, limit)
				return
			}

			// Wrap the response writer to roll back the increment on failure
			wrapped := &rateLimitResponseWriter{
				ResponseWriter: w,
				counter:        counter,
				clientID:       clientID,
			}

			next.ServeHTTP(wrapped, r)
		})
	}
}

// rateLimitResponseWriter wraps http.ResponseWriter to decrement the counter
// when the handler returns a non-2xx response (job wasn't actually created).
type rateLimitResponseWriter struct {
	http.ResponseWriter
	counter  ClientJobCounter
	clientID string
	written  bool
}

func (rw *rateLimitResponseWriter) WriteHeader(statusCode int) {
	if !rw.written {
		rw.written = true
		if statusCode < 200 || statusCode >= 300 {
			rw.counter.Decrement(rw.clientID)
		}
	}
	rw.ResponseWriter.WriteHeader(statusCode)
}

func (rw *rateLimitResponseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.written = true
	}
	return rw.ResponseWriter.Write(b)
}

func writeRateLimitResponse(w http.ResponseWriter, current, limit int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusTooManyRequests)

	resp := protocol.RateLimitResponse{
		Code:    protocol.ErrCodeRateLimitExceeded,
		Message: "Too many concurrent jobs. Please wait for existing jobs to complete before submitting new ones.",
		Detail:  "The number of active jobs for this client exceeds the configured limit.",
		Current: current,
		Limit:   limit,
		RetryIn: 5,
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to encode rate limit response: %v", err)
	}
}
