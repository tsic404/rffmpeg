package handlers

import (
	"path/filepath"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestClassifyProbeInput pins the closed probe input taxonomy (C-123 / C-51):
// an upload identifier, an absolute http(s) URL, or an absolute path inside the
// declared shared filesystem space. Anything else — a relative path, a path
// outside every declared root, a path carrying "..", or another scheme — is a
// form error, hence invalid_request. A well-formed identifier with no stored
// file stays a file ID here; the handler maps that to not_found.
func TestClassifyProbeInput(t *testing.T) {
	root := t.TempDir()
	h := &Handler{}
	h.SetSharedFSRoots(root)

	tests := []struct {
		name    string
		input   string
		want    probeInputClass
		wantErr protocol.ErrorCode
	}{
		{"file id", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", probeInputFileID, ""},
		{"remote url", "https://example.test/in.mp4", probeInputRemoteURL, ""},
		{"url with dotdot segment", "https://example.test/a/../b.mp4", probeInputRemoteURL, ""},
		{"absolute path in the declared space", filepath.Join(root, "in.mp4"), probeInputPath, ""},
		{"path containing scheme marker", filepath.Join(root, "x://y"), probeInputPath, ""},
		{"path with untouched filename dots", filepath.Join(root, "a..b.mp4"), probeInputPath, ""},
		{"relative path", "media/in.mp4", probeInputPath, protocol.ErrCodeInvalidRequest},
		{"absolute path outside every root", "/data/media/in.mp4", probeInputPath, protocol.ErrCodeInvalidRequest},
		{"traversal inside the root", root + "/sub/../media.mp4", probeInputPath, protocol.ErrCodeInvalidRequest},
		{"non-http scheme", "rtmp://example.test/live", probeInputPath, protocol.ErrCodeInvalidRequest},
		{"file scheme", "file:///etc/passwd", probeInputPath, protocol.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, perr := h.classifyProbeInput(tt.input)
			if got != tt.want {
				t.Errorf("class = %v, want %v", got, tt.want)
			}
			if tt.wantErr == "" {
				if perr != nil {
					t.Fatalf("unexpected error: %v", perr)
				}
				return
			}
			if perr == nil {
				t.Fatalf("want %s, got no error", tt.wantErr)
			}
			if perr.Code != tt.wantErr {
				t.Errorf("code = %s, want %s", perr.Code, tt.wantErr)
			}
		})
	}
}

// TestClassifyProbeInputUndeclaredSpace locks the fail-closed default: with no
// root declared the path form does not exist, so an absolute path is a form
// error rather than an unchecked local read.
func TestClassifyProbeInputUndeclaredSpace(t *testing.T) {
	h := &Handler{}

	if _, perr := h.classifyProbeInput("/data/media/in.mp4"); perr == nil || perr.Code != protocol.ErrCodeInvalidRequest {
		t.Fatalf("absolute path with an undeclared space = %v, want invalid_request", perr)
	}
}
