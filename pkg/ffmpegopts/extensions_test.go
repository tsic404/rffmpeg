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

// TestMuxerForExtension pins the muxer ffmpeg guesses for common output
// extensions: the guess decides which default video codec family a request
// without an explicit encoder targets, so a regeneration that swaps e.g. webm
// for matroska would silently change the encoder family.
func TestMuxerForExtension(t *testing.T) {
	cases := map[string]string{
		".mp4":  "mp4",
		".mkv":  "matroska",
		".webm": "webm",
		".mov":  "mov",
		".avi":  "avi",
		".ts":   "mpegts",
		".gif":  "gif",
		".m4a":  "ipod",
		// Collisions resolved by registration order, not by muxer name:
		// ipod is registered before the raw m4v muxer, svcd before vob.
		".m4v":  "ipod",
		".vob":  "svcd",
		".wav":  "wav",
		".mp3":  "mp3",
		".flac": "flac",
		".opus": "opus",
	}
	for ext, want := range cases {
		if got := MuxerForExtension(ext); got != want {
			t.Errorf("MuxerForExtension(%q) = %q, want %q", ext, got, want)
		}
	}
	if got := MuxerForExtension(".out"); got != "" {
		t.Errorf(`MuxerForExtension(".out") = %q, want ""`, got)
	}
}

// TestDefaultVideoCodec covers the two questions the rewrite engine asks of a
// muxer: which codec family native ffmpeg would pick, and whether the muxer
// carries video at all.
func TestDefaultVideoCodec(t *testing.T) {
	cases := []struct {
		muxer     string
		wantCodec string
		wantOK    bool
	}{
		{"mp4", "h264", true},
		{"matroska", "h264", true},
		{"webm", "vp9", true},
		{"gif", "gif", true},
		{"mpegts", "mpeg2video", true},
		// Audio-only containers carry no video stream.
		{"wav", "", false},
		{"opus", "", false},
		{"unknown-muxer", "", false},
	}
	for _, tc := range cases {
		codec, ok := DefaultVideoCodec(tc.muxer)
		if codec != tc.wantCodec || ok != tc.wantOK {
			t.Errorf("DefaultVideoCodec(%q) = (%q, %v), want (%q, %v)",
				tc.muxer, codec, ok, tc.wantCodec, tc.wantOK)
		}
	}
}

// TestGeneratedTablesNotEmpty guards against an accidentally shrunken
// generated table: representative entries from every major category must
// resolve, so a regeneration that silently drops e.g. all audio formats is
// caught instead of only a missing mp4.
func TestGeneratedTablesNotEmpty(t *testing.T) {
	// video / container
	for _, ext := range []string{"mp4", "mkv", "mov", "webm", "avi", "ts", "flv", "m3u8"} {
		if _, ok := extensionMuxer[ext]; !ok {
			t.Errorf("extensionMuxer missing video/container extension %q", ext)
		}
	}
	// audio
	for _, ext := range []string{"mp3", "wav", "flac", "aac", "ogg", "m4a", "ac3", "opus"} {
		if _, ok := extensionMuxer[ext]; !ok {
			t.Errorf("extensionMuxer missing audio extension %q", ext)
		}
	}
	// image (image2 muxer)
	for _, ext := range []string{"png", "jpg", "bmp", "tiff", "webp"} {
		if _, ok := extensionMuxer[ext]; !ok {
			t.Errorf("extensionMuxer missing image extension %q", ext)
		}
	}
	// subtitle
	for _, ext := range []string{"srt", "ass", "vtt"} {
		if _, ok := extensionMuxer[ext]; !ok {
			t.Errorf("extensionMuxer missing subtitle extension %q", ext)
		}
	}
	if _, ok := extensionMuxer["out"]; ok {
		t.Errorf(`extensionMuxer contains "out"; it is not a real muxer extension`)
	}
	for _, muxer := range []string{"mp4", "matroska", "webm", "mpegts", "gif"} {
		if _, ok := muxerDefaultVideoCodec[muxer]; !ok {
			t.Errorf("muxerDefaultVideoCodec missing muxer %q", muxer)
		}
	}
}
