package main

import (
	"strings"
	"testing"
)

func TestPlaintextRemoteServerWarning(t *testing.T) {
	tests := []struct {
		url      string
		wantLoud bool
	}{
		{"http://localhost:8080", false},
		{"http://localhost.:8080", false},
		{"http://127.0.0.1:8080", false},
		{"http://127.0.0.2:8080", false},
		{"http://[::1]:8080", false},
		{"https://gpu.example.test:8443", false},
		{"http://gpu.example.test:8080", true},
		{"http://192.168.1.20:8080", true},
	}
	for _, tt := range tests {
		got := plaintextRemoteServerWarning(tt.url)
		if loud := got != ""; loud != tt.wantLoud {
			t.Errorf("plaintextRemoteServerWarning(%q) = %q, want warning=%v", tt.url, got, tt.wantLoud)
		}
	}
}

// TestPlaintextRemoteServerWarningHidesUserinfo covers a URL carrying
// credentials: the warning must not echo them into logs or terminals.
func TestPlaintextRemoteServerWarningHidesUserinfo(t *testing.T) {
	got := plaintextRemoteServerWarning("http://alice:s3cret@203.0.113.9:8080/api/v1")
	if got == "" {
		t.Fatal("expected a plaintext warning")
	}
	if strings.Contains(got, "s3cret") || strings.Contains(got, "alice") {
		t.Fatalf("warning leaks userinfo: %q", got)
	}
	if !strings.Contains(got, "203.0.113.9") {
		t.Fatalf("warning does not name the host: %q", got)
	}
}
