package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// TestIntegration_AuthFlow tests the complete auth flow similar to main.go setup
func TestIntegration_AuthFlow(t *testing.T) {
	// Setup router exactly like main.go
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)

	// Add auth middleware with token (simulating --auth-token flag)
	token := "test-token-123"
	r.Use(Middleware(token))

	// Protected endpoints - exactly as defined in main.go
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/upload", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"file_id":"test123"}`))
		})
		r.Post("/jobs", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"job_id":"job123"}`))
		})
		r.Get("/jobs/{jobId}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"job":{"id":"job123","status":"pending"}}`))
		})
		r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		})
	})

	t.Run("HealthEndpoint_NoAuth_Required", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/health", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Health endpoint should not require auth, got status %d", rec.Code)
		}
	})

	t.Run("UploadEndpoint_NoAuth_ShouldFail", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/upload", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Upload without auth should return 401, got status %d", rec.Code)
		}
	})

	t.Run("JobsEndpoint_NoAuth_ShouldFail", func(t *testing.T) {
		body := strings.NewReader(`{"input_files":["test.mp4"],"args":["-c:v","libx264"],"output_filename":"out.mp4"}`)
		req := httptest.NewRequest("POST", "/api/v1/jobs", body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Job submission without auth should return 401, got status %d", rec.Code)
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil {
			if resp["code"] != "unauthorized" {
				t.Errorf("Expected error code 'unauthorized', got '%v'", resp["code"])
			}
		}
	})

	t.Run("JobsEndpoint_ValidAuth_ShouldSucceed", func(t *testing.T) {
		body := strings.NewReader(`{"input_files":["test.mp4"],"args":["-c:v","libx264"],"output_filename":"out.mp4"}`)
		req := httptest.NewRequest("POST", "/api/v1/jobs", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token-123")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Job submission with valid auth should return 200, got status %d", rec.Code)
		}
	})

	t.Run("JobsEndpoint_WrongAuth_ShouldFail", func(t *testing.T) {
		body := strings.NewReader(`{"input_files":["test.mp4"],"args":["-c:v","libx264"],"output_filename":"out.mp4"}`)
		req := httptest.NewRequest("POST", "/api/v1/jobs", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer wrong-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Job submission with wrong auth should return 401, got status %d", rec.Code)
		}
	})

	t.Run("GetJobEndpoint_NoAuth_ShouldFail", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/jobs/job123", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Get job without auth should return 401, got status %d", rec.Code)
		}
	})

	t.Run("GetJobEndpoint_ValidAuth_ShouldSucceed", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/jobs/job123", nil)
		req.Header.Set("Authorization", "Bearer test-token-123")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Get job with valid auth should return 200, got status %d", rec.Code)
		}
	})
}

// TestIntegration_NoAuthToken tests server without auth token configured
func TestIntegration_NoAuthToken(t *testing.T) {
	// Setup router without auth token
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// No auth middleware added (simulating no --auth-token flag)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/jobs", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"job_id":"job123"}`))
		})
	})

	t.Run("JobsEndpoint_NoAuthMiddleware_ShouldSucceed", func(t *testing.T) {
		body := strings.NewReader(`{"input_files":["test.mp4"],"args":["-c:v","libx264"],"output_filename":"out.mp4"}`)
		req := httptest.NewRequest("POST", "/api/v1/jobs", body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Job submission without auth middleware should return 200, got status %d", rec.Code)
		}
	})
}
