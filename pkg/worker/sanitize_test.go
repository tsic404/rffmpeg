package worker

import (
	"strings"
	"testing"
)

// TestSanitizeInputBaseName verifies the charset/length restrictions applied
// to remote-URL-derived input filenames.
func TestSanitizeInputBaseName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"video.mp4", "video.mp4"},
		{"my file (1).mkv", "my file (1).mkv"},
		// Non-ASCII URL basenames must not survive into the local filename:
		// CJK runes collapse to '_' while the ASCII space is preserved, so a
		// raw UTF-8/space name and its percent-encoded spelling both yield a
		// safe path ffmpeg can read.
		{"电影 4K.mkv", "__ 4K.mkv"},
		{"movie%204K.mkv", "movie_204K.mkv"},
		{"%E7%94%B5%E5%BD%B1.mkv", "_E7_94_B5_E5_BD_B1.mkv"},
		{"", ""},
		{".", ""},  // falls back to UUID
		{"..", ""}, // falls back to UUID
		{"/", ""},  // falls back to UUID
	}
	for _, tc := range cases {
		got := sanitizeInputBaseName(tc.in)
		if tc.want == "" {
			if got == "" || got == "." || got == ".." || strings.ContainsAny(got, "/\\") {
				t.Errorf("sanitizeInputBaseName(%q) = %q, want a safe UUID fallback", tc.in, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("sanitizeInputBaseName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Path separators must never survive.
	if got := sanitizeInputBaseName("..%2F..%2Fetc%2Fpasswd"); strings.Contains(got, "/") {
		t.Errorf("path separator survived sanitization: %q", got)
	}

	// Length must be bounded.
	long := strings.Repeat("a", 10000)
	if got := sanitizeInputBaseName(long); len(got) > maxInputBaseName {
		t.Errorf("sanitized name too long: %d > %d", len(got), maxInputBaseName)
	}
}
