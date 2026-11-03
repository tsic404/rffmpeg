package main

import (
	"bytes"
	"crypto/tls"
	"log"
	"strings"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/config"
	tlspkg "github.com/tsic404/rffmpeg/pkg/tls"
)

// TestWarnInsecureTransport pins the startup warnings: plaintext serving and a
// minimum TLS version below 1.2 must be called out, while TLS 1.2+ stays
// silent.
func TestWarnInsecureTransport(t *testing.T) {
	var buf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(origWriter) })

	tests := []struct {
		name    string
		cfg     *config.ServerConfig
		wantMsg string
	}{
		{
			name:    "plaintext",
			cfg:     &config.ServerConfig{TLS: &tlspkg.Config{Enabled: false}},
			wantMsg: "TLS is disabled",
		},
		{
			name:    "tls10",
			cfg:     &config.ServerConfig{TLS: &tlspkg.Config{Enabled: true, MinVersion: tls.VersionTLS10}},
			wantMsg: "versions below TLS 1.2",
		},
		{
			name:    "tls12",
			cfg:     &config.ServerConfig{TLS: &tlspkg.Config{Enabled: true, MinVersion: tls.VersionTLS12}},
			wantMsg: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			warnInsecureTransport(tt.cfg)
			got := buf.String()
			if tt.wantMsg == "" {
				if strings.Contains(got, "WARNING") {
					t.Fatalf("unexpected warning for TLS 1.2: %s", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantMsg) {
				t.Fatalf("warning %q does not mention %q", got, tt.wantMsg)
			}
		})
	}
}
