package handlers

import (
	"path/filepath"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestClassifyProbeInput pins the closed input taxonomy: a valid file ID, a
// remote URL, or a path. Only paths are subject to the traversal check and
// only malformed paths are invalid_request; a well-formed ID is a file ID even
// when no stored file exists (the handler maps that to not_found).
func TestClassifyProbeInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    probeInputClass
		wantErr protocol.ErrorCode
	}{
		{"file id", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", probeInputFileID, ""},
		{"remote url", "https://example.test/in.mp4", probeInputRemoteURL, ""},
		{"url with dotdot segment", "https://example.test/a/../b.mp4", probeInputRemoteURL, ""},
		{"absolute path", "/data/media/in.mp4", probeInputPath, ""},
		{"relative path", "media/in.mp4", probeInputPath, ""},
		{"path containing scheme marker", "/data/media/x://y", probeInputPath, ""},
		{"path with untouched filename dots", "/data/media/a..b.mp4", probeInputPath, ""},
		{
			name:    "traversal",
			input:   "/data/sub/../media.mp4",
			want:    probeInputPath,
			wantErr: protocol.ErrCodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, perr := classifyProbeInput(tt.input)
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

// TestClassifyProbeInputRelativeTraversal covers traversal detection on
// relative paths, which have no separator prefix.
func TestClassifyProbeInputRelativeTraversal(t *testing.T) {
	traversal := "media" + string(filepath.Separator) + ".." + string(filepath.Separator) + "escape.mp4"
	if _, perr := classifyProbeInput(traversal); perr == nil || perr.Code != protocol.ErrCodeInvalidRequest {
		t.Fatalf("relative traversal code = %v, want invalid_request", perr)
	}
}
