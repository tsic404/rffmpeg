package pathutil

import "testing"

func TestContainsPathTraversal(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"empty path", "", false},
		{"plain relative path", "media/video.mp4", false},
		{"absolute path", "/srv/media/video.mp4", false},
		{"backslash separators", `srv\media\video.mp4`, false},
		{"single parent component", "..", true},
		{"leading parent", "../etc/passwd", true},
		{"embedded parent component", "/srv/media/../../etc/passwd", true},
		{"backslash parent component", `srv\..\etc\passwd`, true},
		{"trailing parent component", "/srv/media/..", true},
		{"dot-prefixed filename", "my..video.mp4", false},
		{"embedded dots filename", "a..b/c.mp4", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsPathTraversal(tt.path); got != tt.want {
				t.Errorf("ContainsPathTraversal(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestIsRemoteURL locks the TSI-2646 fix: URL detection must anchor the scheme
// at the start of the string AND exclude the local "file" scheme. A "://"
// appearing mid-string is part of a local path component, not a remote URL, so
// it must never bypass the shared-FS allow-list validation; a leading "file://"
// addresses a local path via ffmpeg's file protocol and is likewise not remote.
func TestIsRemoteURL(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{name: "http", s: "http://example.com/video.mp4", want: true},
		{name: "https", s: "https://cdn.example.com/a/b/c.mkv", want: true},
		{name: "ftp", s: "ftp://files.example.com/media.avi", want: true},
		{name: "rtmp", s: "rtmp://server/live/stream", want: true},
		{name: "srt", s: "srt://host:9000?mode=listener", want: true},
		{name: "file", s: "file:///data/media/in.mp4", want: false},
		{name: "file uppercase", s: "FILE:///data/media/in.mp4", want: false},
		{name: "file single slash", s: "file:/data/media/in.mp4", want: false},
		{name: "udp", s: "udp://239.0.0.1:1234", want: true},
		{name: "scheme with plus", s: "git+ssh://host/repo.git", want: true},

		{name: "absolute local path with infix", s: "/abs/path://evil", want: false},
		{name: "absolute local path with colon component", s: "/data/x:foo", want: false},
		{name: "absolute local path colon then slashes", s: "/data/media/x://out.mp4", want: false},
		{name: "plain server file id", s: "abc123", want: false},
		{name: "uuid file id", s: "0f8b3c2e-1d4a-4e5f-9a6b-upload0042", want: false},
		{name: "empty", s: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRemoteURL(tt.s); got != tt.want {
				t.Errorf("IsRemoteURL(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}
