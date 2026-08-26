package worker

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// TestSplitProgressLinesCarriageReturns verifies that ffmpeg's in-place
// rewritten -stats line (updates separated by \r) yields one token per
// update instead of a single token at the next \n. Regression for TSI-2425:
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
	stream := "" +
		"Input #0, mov,mp4:\n  Duration: N/A, start: 0.000000\n" + // unseekable input: no usable header duration
		"frame=  17 fps=0.0 q=16.0 time=00:00:00.68 bitrate=0.6kbits/s speed=1.36x\r" +
		"frame=  55 fps=27 q=16.0 time=00:00:02.20 bitrate=0.2kbits/s speed=1.10x\r" +
		"frame= 130 fps=26 q=16.0 time=00:00:05.20 bitrate=0.2kbits/s speed=1.05x\r" +
		"frame= 200 fps=26 q=-1.0 time=00:00:08.00 Lspeed=1.00x    \n"

	parser := NewProgressParser()
	parser.SetDuration(10 * 1_000_000) // what probeInputDurationUs seeds

	sends := 0

	var lastFrame *ProgressFrame
	handler := func(line string) {
		if !FilterProgressLine(line) {
			return
		}
		f := parser.ParseLine(line)
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
	if sends < 3 {
		t.Fatalf("expected >=3 live progress updates from CR stream, got %d — updates are still being swallowed", sends)
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
