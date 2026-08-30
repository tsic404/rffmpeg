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
