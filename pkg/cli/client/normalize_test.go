package client

import "testing"

func TestNormalizeServerURL(t *testing.T) {
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
		if got := normalizeServerURL(tt.in); got != tt.want {
			t.Errorf("normalizeServerURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
