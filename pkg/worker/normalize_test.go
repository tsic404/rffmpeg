package worker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"http://localhost:8080", "http://localhost:8080"},
		{"http://localhost:8080/", "http://localhost:8080"},
		{"http://localhost:8080/api/v1", "http://localhost:8080"},
		{"http://localhost:8080/api/v1/", "http://localhost:8080"},
		{"https://rffmpeg.example.com:8443/base/api/v1", "https://rffmpeg.example.com:8443/base"},
		{"http://host/API/V1", "http://host/API/V1"},        // case-sensitive: only exact /api/v1 is stripped
		{"http://host/api/v2", "http://host/api/v2"},        // other API versions untouched
		{"http://host/api/v1/api/v1", "http://host/api/v1"}, // strip once only
	}
	for _, tt := range tests {
		if got := normalizeBaseURL(tt.in); got != tt.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestNewClientDoesNotDoubleAPIPrefix is the TSI-2636 regression test: a
// user-supplied "http://host/api/v1" must produce
// "http://host/api/v1/workers/register", not
// "http://host/api/v1/api/v1/workers/register" (which 404s).
func TestNewClientDoesNotDoubleAPIPrefix(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL+"/api/v1", "w1", "")
	resp, err := client.doRequest("POST", "/workers/register", nil)
	if err != nil {
		t.Fatalf("doRequest failed: %v", err)
	}
	defer resp.Body.Close()

	const want = "/api/v1/workers/register"
	if gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
}
