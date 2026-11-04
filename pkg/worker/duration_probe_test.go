package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLongestProbedDurationUs covers the cover-image case: the first input is
// a one-frame image and the audio behind it is the real timeline. The
// denominator must come from the longest input, otherwise the first stats
// line reports 100% and ETA stays n/a for the whole job.
func TestLongestProbedDurationUs(t *testing.T) {
	dir := t.TempDir()
	cover := filepath.Join(dir, "cover.jpg")
	audio := filepath.Join(dir, "audio.mp3")
	for _, p := range []string{cover, audio} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	durations := map[string]float64{
		cover: 0.04,
		audio: 10.0,
	}
	probe := func(_ context.Context, p string) (*FFprobeResult, error) {
		return &FFprobeResult{Format: map[string]interface{}{"duration": durations[p]}}, nil
	}

	got := longestProbedDurationUs(context.Background(), []string{cover, audio}, probe)
	if want := int64(10_000_000); got != want {
		t.Fatalf("longestProbedDurationUs = %d, want %d", got, want)
	}

	parser := NewProgressParser()
	parser.SetDuration(got)
	frame := parser.ParseLine("frame=    1 fps=1.0 q=0.0 size=1kB time=00:00:00.04 bitrate=1.0kbits/s speed=1.0x")
	if frame == nil {
		t.Fatal("ParseLine returned nil for a stats line")
	}
	if frame.Percent >= 100 {
		t.Fatalf("percent = %v, want below 100 with the audio timeline as denominator", frame.Percent)
	}
}

// TestLongestProbedDurationUsStringDuration covers ffprobe's string form and
// inputs that cannot be probed (missing files, option placeholders).
func TestLongestProbedDurationUsStringDuration(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	probe := func(_ context.Context, p string) (*FFprobeResult, error) {
		if p == a {
			return &FFprobeResult{Format: map[string]interface{}{"duration": "3.5"}}, nil
		}
		return &FFprobeResult{Format: map[string]interface{}{"duration": "12.25"}}, nil
	}

	got := longestProbedDurationUs(context.Background(), []string{"<INPUT_FILE>", a, b, filepath.Join(dir, "missing.mp4")}, probe)
	if want := int64(12_250_000); got != want {
		t.Fatalf("longestProbedDurationUs = %d, want %d", got, want)
	}
}
