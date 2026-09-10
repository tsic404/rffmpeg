package ffmpegopts

import "testing"

func TestIsKnownOutputExtension(t *testing.T) {
	cases := []struct {
		ext  string
		want bool
	}{
		{"mp4", true},
		{".mp4", true},
		{"MP4", true},
		{".Mkv", true},
		{"ts", true},
		{"out", false},
		{".out", false},
		{"", false},
		{".", false},
	}
	for _, tc := range cases {
		if got := IsKnownOutputExtension(tc.ext); got != tc.want {
			t.Errorf("IsKnownOutputExtension(%q) = %v, want %v", tc.ext, got, tc.want)
		}
	}
}

// TestKnownOutputExtensionsNotEmpty guards against an accidentally shrunken
// generated table: representative extensions from every major category must
// resolve, so a regeneration that silently drops e.g. all audio formats is
// caught instead of only a missing mp4.
func TestKnownOutputExtensionsNotEmpty(t *testing.T) {
	// video / container
	for _, ext := range []string{"mp4", "mkv", "mov", "webm", "avi", "ts", "flv", "m3u8"} {
		if !knownOutputExtensions[ext] {
			t.Errorf("knownOutputExtensions missing video/container extension %q", ext)
		}
	}
	// audio
	for _, ext := range []string{"mp3", "wav", "flac", "aac", "ogg", "m4a", "ac3", "opus"} {
		if !knownOutputExtensions[ext] {
			t.Errorf("knownOutputExtensions missing audio extension %q", ext)
		}
	}
	// image (image2 muxer)
	for _, ext := range []string{"png", "jpg", "bmp", "tiff", "webp"} {
		if !knownOutputExtensions[ext] {
			t.Errorf("knownOutputExtensions missing image extension %q", ext)
		}
	}
	// subtitle
	for _, ext := range []string{"srt", "ass", "vtt"} {
		if !knownOutputExtensions[ext] {
			t.Errorf("knownOutputExtensions missing subtitle extension %q", ext)
		}
	}
	if knownOutputExtensions["out"] {
		t.Errorf(`knownOutputExtensions contains "out"; it is not a real muxer extension`)
	}
}
