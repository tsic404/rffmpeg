package worker

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestSplitProgressLinesCarriageReturns verifies that ffmpeg's in-place
// rewritten -stats line (updates separated by \r) yields one token per
// update instead of a single token at the next \n. Regression for:
// the default ScanLines starved ProgressRouter so server-side progress
// pushes only fired once per job.
func TestSplitProgressLinesCarriageReturns(t *testing.T) {
	input := "header\n" +
		"frame=  17 fps=0.0 time=00:00:00.68 speed=1.36x\r" +
		"frame=  55 fps=27   time=00:00:02.20 speed=1.10x\r" +
		"frame= 130 fps=26   time=00:00:05.20 speed=1.05x\n" +
		"tail\n"

	var lines []string
	sc := bufio.NewScanner(bytes.NewReader([]byte(input)))
	sc.Split(splitProgressLines)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	want := []string{
		"header",
		"frame=  17 fps=0.0 time=00:00:00.68 speed=1.36x",
		"frame=  55 fps=27   time=00:00:02.20 speed=1.10x",
		"frame= 130 fps=26   time=00:00:05.20 speed=1.05x",
		"tail",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %q", len(lines), len(want), lines)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d: got %q, want %q", i, lines[i], w)
		}
	}
}

// TestSplitProgressLinesCRLFPair ensures a Windows-style "\r\n" terminator
// produces one separator, not two empty tokens.
func TestSplitProgressLinesCRLFPair(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("a\r\nb\r\n"))
	sc.Split(splitProgressLines)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 2 || lines[0] != "a" || lines[1] != "b" {
		t.Fatalf("got %q, want [a b]", lines)
	}
}

// TestProgressRouterLiveUpdatesFromCRStream drives the full stderr path:
// \r-separated stats lines must produce multiple throttled progress frames
// with percent and ETA populated from a seeded duration.
func TestProgressRouterLiveUpdatesFromCRStream(t *testing.T) {
	// One stats line per wall second with a steady 1.9s/s pace: dense enough
	// for the rate window to measure a pace the estimate can certify, and long
	// enough (20.9s of media at the last line) to be past the settle gate.
	stream := "" +
		"Input #0, mov,mp4:\n  Duration: N/A, start: 0.000000\n" + // unseekable input: no usable header duration
		"frame=   0 fps=0.0 q=16.0 time=00:00:00.00 bitrate=0.6kbits/s speed=1.00x\r" +
		"frame=  30 fps=26 q=16.0 time=00:00:01.90 bitrate=0.2kbits/s speed=1.00x\r" +
		"frame=  60 fps=26 q=16.0 time=00:00:03.80 bitrate=0.2kbits/s speed=1.00x\r" +
		"frame=  90 fps=26 q=16.0 time=00:00:05.70 bitrate=0.2kbits/s speed=1.00x\r" +
		"frame= 120 fps=26 q=-1.0 time=00:00:07.60 Lspeed=1.00x\r" +
		"frame= 150 fps=26 q=-1.0 time=00:00:09.50 Lspeed=1.00x\r" +
		"frame= 180 fps=26 q=-1.0 time=00:00:11.40 Lspeed=1.00x\r" +
		"frame= 210 fps=26 q=-1.0 time=00:00:13.30 Lspeed=1.00x\r" +
		"frame= 240 fps=26 q=-1.0 time=00:00:15.20 Lspeed=1.00x\r" +
		"frame= 270 fps=26 q=-1.0 time=00:00:17.10 Lspeed=1.00x\r" +
		"frame= 300 fps=26 q=-1.0 time=00:00:19.00 Lspeed=1.00x\r" +
		"frame= 330 fps=26 q=-1.0 time=00:00:20.90 Lspeed=1.00x    \n"

	parser := NewProgressParser()
	parser.SetDuration(120 * 1_000_000) // what probeInputDurationUs seeds

	// Inject wall-clock time: each stats line arrives one second apart. The
	// ETA needs a rate window and enough published media to be past the
	// start-up ramp, which the last line provides.
	wallSec := 0.0
	parser.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*1e9))
	}
	sends := 0

	var lastFrame *ProgressFrame
	handler := func(line string) {
		if !FilterProgressLine(line) {
			return
		}
		f := parser.ParseLine(line)
		wallSec += 1.0
		if f == nil || f.Percent < 0 {
			return
		}
		sends++
		lastFrame = f
	}

	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Split(splitProgressLines)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		handler(line)
	}

	// The old ScanLines behavior produced exactly ONE progress push (the
	// final \n-terminated line). The fix must surface every intermediate
	// update.
	if sends != 12 {
		t.Fatalf("expected every one of the 12 stats lines to surface, got %d — updates are still being swallowed", sends)
	}
	if lastFrame.Percent <= 0 || lastFrame.Percent > 100 {
		t.Fatalf("final percent %.1f out of range", lastFrame.Percent)
	}
	if lastFrame.EtaSeconds <= 0 && lastFrame.TimeUs < lastFrame.DurationUs {
		t.Fatalf("ETA not computed mid-stream (eta=%d, time=%d, dur=%d)",
			lastFrame.EtaSeconds, lastFrame.TimeUs, lastFrame.DurationUs)
	}
	t.Logf("live updates=%d final percent=%.1f eta=%ds", sends, lastFrame.Percent, lastFrame.EtaSeconds)
}
