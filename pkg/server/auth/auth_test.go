package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMiddleware_NoToken tests that ALL requests are rejected when no token is
// configured — including requests without an Authorization header (fail closed).
func TestMiddleware_NoToken(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached when no auth token is configured")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	authMiddleware := Middleware("")
	protectedHandler := authMiddleware(handler)

	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()

	protectedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rec.Code)
	}
}

// TestMiddleware_ExemptPath tests that exempt paths don't require authentication
func TestMiddleware_ExemptPath(t *testing.T) {
	token := "test-token-123"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	authMiddleware := Middleware(token)
	protectedHandler := authMiddleware(handler)

	exemptEndpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/health"},
		{"GET", "/api/v1/health"},
		{"POST", "/api/v1/workers/register"},
		{"POST", "/api/v1/workers/heartbeat"},
	}

	for _, ep := range exemptEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			rec := httptest.NewRecorder()
			protectedHandler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("Exempt endpoint %s %s should be accessible without auth, got status %d", ep.method, ep.path, rec.Code)
			}
		})
	}
}

// TestMiddleware_MissingAuthHeader tests 401 response when auth header is missing
func TestMiddleware_MissingAuthHeader(t *testing.T) {
	token := "test-token-123"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	authMiddleware := Middleware(token)
	protectedHandler := authMiddleware(handler)

	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	protectedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rec.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if resp["code"] != "unauthorized" {
		t.Errorf("Expected error code 'unauthorized', got '%s'", resp["code"])
	}
}

// TestMiddleware_InvalidAuthFormat tests 401 response when auth header format is invalid
func TestMiddleware_InvalidAuthFormat(t *testing.T) {
	token := "test-token-123"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	authMiddleware := Middleware(token)
	protectedHandler := authMiddleware(handler)

	tests := []struct {
		name   string
		header string
	}{
		{"No Bearer prefix", "test-token-123"},
		{"Wrong prefix", "Basic test-token-123"},
		{"Empty Bearer", "Bearer "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
			req.Header.Set("Authorization", tt.header)
			rec := httptest.NewRecorder()
			protectedHandler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("Expected status 401 for %s, got %d", tt.name, rec.Code)
			}
		})
	}
}

// TestMiddleware_InvalidToken tests 401 response when token is invalid
func TestMiddleware_InvalidToken(t *testing.T) {
	token := "test-token-123"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	authMiddleware := Middleware(token)
	protectedHandler := authMiddleware(handler)

	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	protectedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rec.Code)
	}
}

// TestMiddleware_ValidToken tests that valid token passes through
func TestMiddleware_ValidToken(t *testing.T) {
	token := "test-token-123"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID := GetClientID(r)
		if clientID == "" {
			t.Error("Client ID should be set in context")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	authMiddleware := Middleware(token)
	protectedHandler := authMiddleware(handler)

	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	protectedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}
	if rec.Body.String() != "OK" {
		t.Errorf("Expected body 'OK', got '%s'", rec.Body.String())
	}
}

// TestGenerateClientID tests the client ID generation
func TestGenerateClientID(t *testing.T) {
	token1 := "test-token-1"
	token2 := "test-token-2"

	id1 := GenerateClientID(token1)
	id2 := GenerateClientID(token2)

	if len(id1) != 16 {
		t.Errorf("Expected client ID length 16, got %d", len(id1))
	}

	if id1 == id2 {
		t.Error("Different tokens should produce different client IDs")
	}

	id1Again := GenerateClientID(token1)
	if id1 != id1Again {
		t.Error("Same token should produce same client ID")
	}
}

// TestGetClientID tests extracting client ID from request context
func TestGetClientID(t *testing.T) {
	token := "test-token-123"
	expectedClientID := GenerateClientID(token)

	var actualClientID string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actualClientID = GetClientID(r)
		w.WriteHeader(http.StatusOK)
	})

	authMiddleware := Middleware(token)
	protectedHandler := authMiddleware(handler)

	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	protectedHandler.ServeHTTP(rec, req)

	if actualClientID != expectedClientID {
		t.Errorf("Expected client ID '%s', got '%s'", expectedClientID, actualClientID)
	}
}

// TestGetClientID_NoContext tests GetClientID when no client ID is in context
func TestGetClientID_NoContext(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	clientID := GetClientID(req)

	if clientID != "" {
		t.Errorf("Expected empty client ID, got '%s'", clientID)
	}
}

// TestMiddleware_BearerCaseInsensitive tests that "Bearer" prefix is case-insensitive
func TestMiddleware_BearerCaseInsensitive(t *testing.T) {
	token := "test-token-123"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	authMiddleware := Middleware(token)
	protectedHandler := authMiddleware(handler)

	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "bearer "+token)
	rec := httptest.NewRecorder()
	protectedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200 with lowercase bearer, got %d", rec.Code)
	}
}

// TestMiddleware_EmptyToken tests 401 response when token is empty after "Bearer "
func TestMiddleware_EmptyToken(t *testing.T) {
	token := "test-token-123"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	authMiddleware := Middleware(token)
	protectedHandler := authMiddleware(handler)

	tests := []struct {
		name   string
		header string
	}{
		{"Empty token after Bearer", "Bearer "},
		{"Only Bearer", "Bearer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
			req.Header.Set("Authorization", tt.header)
			rec := httptest.NewRecorder()
			protectedHandler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("Expected status 401 for %s, got %d", tt.name, rec.Code)
			}

			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil {
				if resp["code"] != "unauthorized" {
					t.Errorf("Expected error code 'unauthorized', got '%s'", resp["code"])
				}
			}
		})
	}
}
