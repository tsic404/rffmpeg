package main

import "testing"

func TestPlaintextRemoteServerWarning(t *testing.T) {
	tests := []struct {
		url      string
		wantLoud bool
	}{
		{"http://localhost:8080", false},
		{"http://127.0.0.1:8080", false},
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
