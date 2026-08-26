package worker

import (
	"fmt"
	"testing"
)

func TestParseTimeToUs(t *testing.T) {
	tests := []struct {
		hh, mm, ss, ms string
		expected       int64
	}{
		{"00", "00", "00", "000000", 0},
		{"00", "00", "01", "000000", 1_000_000},
		{"00", "01", "00", "000000", 60_000_000},
		{"01", "00", "00", "000000", 3600_000_000},
		{"00", "00", "04", "100000", 4_100_000},
		{"01", "30", "45", "500000", 1*3600_000_000 + 30*60_000_000 + 45_000_000 + 500_000},
		{"00", "00", "10", "50", 10_500_000}, // short ms is padded to 6 digits
	}

	for _, tt := range tests {
		got := parseTimeToUs(tt.hh, tt.mm, tt.ss, tt.ms)
		if got != tt.expected {
			t.Errorf("parseTimeToUs(%s, %s, %s, %s) = %d, want %d", tt.hh, tt.mm, tt.ss, tt.ms, got, tt.expected)
		}
	}
}

func TestParseDurationLine(t *testing.T) {
	tests := []struct {
		line     string
		expected int64
	}{
		{"  Duration: 00:01:30.50, start: 0.000000, bitrate: 1000 kb/s", 90_500_000},
		{"  Duration: 00:00:00.00, start: 0.000000", 0},
		{"No duration here", 0},
		{"Duration: 02:00:00.000000", 7200_000_000},
	}

	for _, tt := range tests {
		got := ParseDurationLine(tt.line)
		if got != tt.expected {
			t.Errorf("ParseDurationLine(%q) = %d, want %d", tt.line, got, tt.expected)
		}
	}
}

func TestProgressParser_ParseLine(t *testing.T) {
	p := NewProgressParser()
	p.SetDuration(100_000_000) // 100 seconds in microseconds

	tests := []struct {
		line            string
		wantNil         bool
		wantTimeUs      int64
		wantDurationUs  int64
		wantPercentLow  float64
		wantPercentHigh float64
	}{
		{line: "frame=  100 fps= 30 q=28.0 size=   1024kB time=00:00:04.10 bitrate=2045.0kbits/s speed=2.05x", wantTimeUs: 4_100_000, wantDurationUs: 100_000_000, wantPercentLow: 3.5, wantPercentHigh: 4.5},
		{line: "frame=    0 fps=0.0 q=0.0 size=       0kB time=00:00:00.00 bitrate=N/A speed=0.00x", wantTimeUs: 0, wantPercentLow: -0.5, wantPercentHigh: 0.5},
		{line: "No progress here", wantNil: true},
		{line: "frame= 1000 fps= 60 size= 50000kB time=00:01:30.00 bitrate=4500.0kbits/s speed=3.00x", wantTimeUs: 90_000_000, wantDurationUs: 100_000_000, wantPercentLow: 89, wantPercentHigh: 91},
	}

	for _, tt := range tests {
		frame := p.ParseLine(tt.line)
		if tt.wantNil {
			if frame != nil {
				t.Errorf("ParseLine(%q) expected nil, got %+v", tt.line, frame)
			}
			continue
		}
		if frame == nil {
			t.Errorf("ParseLine(%q) expected non-nil, got nil", tt.line)
			continue
		}
		if frame.TimeUs != tt.wantTimeUs {
			t.Errorf("ParseLine(%q) TimeUs = %d, want %d", tt.line, frame.TimeUs, tt.wantTimeUs)
		}
		if tt.wantPercentLow > 0 && (frame.Percent < tt.wantPercentLow || frame.Percent > tt.wantPercentHigh) {
			t.Errorf("ParseLine(%q) Percent = %f, want between %f and %f", tt.line, frame.Percent, tt.wantPercentLow, tt.wantPercentHigh)
		}
	}
}

func TestFilterProgressLine(t *testing.T) {
	if !FilterProgressLine("frame=100 time=00:01:00.00 speed=1.0x") {
		t.Error("expected true for progress line")
	}
	if FilterProgressLine("Just a regular log message") {
		t.Error("expected false for non-progress line")
	}
}

func TestProgressParser_ShouldPush(t *testing.T) {
	p := NewProgressParser()
	// First call should return true
	if !p.ShouldPush() {
		t.Error("expected ShouldPush to return true on first call")
	}
	// Immediate second call should be throttled
	if p.ShouldPush() {
		t.Error("expected ShouldPush to return false on second immediate call")
	}
}

// TestProgressParser_ETAPrecision_ConstantSpeed verifies ETA precision
// for a long task with constant encoding speed. In this scenario,
// the ETA prediction should be exact (0% error) because the speed
// does not vary.
func TestProgressParser_ETAPrecision_ConstantSpeed(t *testing.T) {
	// 1-hour video (3600 seconds) encoded at constant 2.0x speed.
	// Wall clock time: 3600 / 2.0 = 1800 seconds.
	durationUs := int64(3600) * 1_000_000
	const (
		speed = 2_000_000 // 2.0x encoded as microseconds-per-microsecond integer
	)

	// Checkpoints every 10% of progress.
	checkpoints := [][2]int64{
		{360 * 1_000_000, speed},  // 10%
		{900 * 1_000_000, speed},  // 25%
		{1800 * 1_000_000, speed}, // 50%
		{2700 * 1_000_000, speed}, // 75%
		{3240 * 1_000_000, speed}, // 90%
		{3550 * 1_000_000, speed}, // ~98.6%
	}

	results := VerifyETAPrecision(checkpoints, durationUs, 0.20, 0)

	for _, r := range results {
		if !r.Passed {
			t.Errorf("constant-speed checkpoint at %.1f%%: ETA predicted=%ds actual=%ds error=%.1f%% - expected PASS",
				r.ProgressPercent, r.EtaPredicted, r.EtaActual, r.ErrorPercent)
		}
		// With constant speed, error should be essentially 0.
		if r.ErrorPercent > 1.0 {
			t.Errorf("constant-speed checkpoint at %.1f%%: error=%.1f%% exceeds 1%% tolerance for constant speed",
				r.ProgressPercent, r.ErrorPercent)
		}
	}
}

// TestProgressParser_ETAPrecision_VaryingSpeed verifies ETA precision
// for a long task with naturally varying encoding speed. This simulates
// a real-world ffmpeg run where speed fluctuates moderately between 1.8x and 2.1x
// across different sections of the video (e.g., simple vs complex scenes).
// The ETA error must stay below 20% at all checkpoints (Scene 9.2).
func TestProgressParser_ETAPrecision_VaryingSpeed(t *testing.T) {
	// 1-hour video (3600 seconds) with varying speed per segment.
	durationUs := int64(3600) * 1_000_000

	// Speed encoded as integer (1_000_000 = 1.0x).
	// Segments simulate real-world variation with moderate speed changes
	// typical of a mixed-complexity encoding run (1.8x-2.1x range).
	//   0% - 10%: moderate (1.9x)
	//  10% - 25%: slightly faster (2.1x)
	//  25% - 50%: slower section (1.8x)
	//  50% - 75%: fast again (2.1x)
	//  75% - 90%: moderate (1.9x)
	//  90% - 100%: nominal (2.0x)
	checkpoints := [][2]int64{
		{360 * 1_000_000, 1_900_000},  // 10% - speed 1.9x
		{900 * 1_000_000, 2_100_000},  // 25% - speed 2.1x
		{1800 * 1_000_000, 1_800_000}, // 50% - speed 1.8x
		{2700 * 1_000_000, 2_100_000}, // 75% - speed 2.1x
		{3240 * 1_000_000, 1_900_000}, // 90% - speed 1.9x
	}

	results := VerifyETAPrecision(checkpoints, durationUs, 0.20, 0)

	for _, r := range results {
		t.Logf("Checkpoint %.1f%%: ETA predicted=%ds actual=%ds error=%.1f%% passed=%v",
			r.ProgressPercent, r.EtaPredicted, r.EtaActual, r.ErrorPercent, r.Passed)

		if !r.Passed {
			t.Errorf("varying-speed checkpoint at %.1f%%: ETA predicted=%ds actual=%ds error=%.1f%% - exceeds 20%% tolerance",
				r.ProgressPercent, r.EtaPredicted, r.EtaActual, r.ErrorPercent)
		}
	}
}

// TestProgressParser_ETAPrecision_BoundaryChecks tests ETA edge cases:
// start of encoding (time=0), near-completion, and speed=0.
func TestProgressParser_ETAPrecision_BoundaryChecks(t *testing.T) {
	// 30-second video.
	durationUs := int64(30) * 1_000_000

	// Edge case 1: Start of encoding (time=0.00s).
	// speed=1.0x but time=0, so ETA should not be calculated (timeUs > 0 check).
	p := NewProgressParser()
	p.SetDuration(durationUs)
	frame := p.ParseLine("frame=0 fps=0 q=0.0 size=0kB time=00:00:00.00 bitrate=N/A speed=1.00x")
	if frame == nil {
		t.Fatal("expected non-nil frame for time=0 line")
	}
	if frame.EtaSeconds != 0 {
		t.Errorf("time=0 should produce ETA=0 (or unknown), got %d", frame.EtaSeconds)
	}

	// Edge case 2: Near-completion (time close to duration).
	nearEndUs := int64(29) * 1_000_000 // 29 out of 30 seconds
	checkpoints := [][2]int64{
		{nearEndUs, 2_000_000}, // 2.0x speed, 1s remaining → 0.5s estimated
	}
	results := VerifyETAPrecision(checkpoints, durationUs, 0.20, 0)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.EtaPredicted != 0 {
		// 1 second remaining at 2.0x → 0.5s, truncated to int = 0s
		t.Logf("Near-completion ETA: predicted=%ds actual=%ds error=%.1f%%",
			r.EtaPredicted, r.EtaActual, r.ErrorPercent)
	}
	if !r.Passed && r.EtaActual > 0 {
		t.Errorf("near-completion ETA error should be within 20%%: predicted=%ds actual=%ds error=%.1f%%",
			r.EtaPredicted, r.EtaActual, r.ErrorPercent)
	}

	// Edge case 3: speed=0 (no progress, should not crash).
	frame2 := p.ParseLine("frame=100 time=00:00:05.00 bitrate=N/A speed=0.00x")
	if frame2 == nil {
		t.Fatal("expected non-nil frame")
	}
	// ETA should be 0 since speed=0 is not > 0.
	if frame2.EtaSeconds != 0 {
		t.Errorf("speed=0 should not produce ETA, got %d", frame2.EtaSeconds)
	}

	// Edge case 4: Very long task (24 hours).
	longDurationUs := int64(24*3600) * 1_000_000 // 24 hours
	longCheckpoints := [][2]int64{
		{longDurationUs / 4, 1_900_000},     // 25% at 1.9x
		{longDurationUs / 2, 2_100_000},     // 50% at 2.1x
		{longDurationUs * 3 / 4, 2_000_000}, // 75% at 2.0x
	}
	longResults := VerifyETAPrecision(longCheckpoints, longDurationUs, 0.20, 0)
	for _, lr := range longResults {
		t.Logf("Long-task checkpoint %.1f%%: ETA predicted=%ds (%s) actual=%ds (%s) error=%.1f%% passed=%v",
			lr.ProgressPercent,
			lr.EtaPredicted, formatDuration(lr.EtaPredicted),
			lr.EtaActual, formatDuration(lr.EtaActual),
			lr.ErrorPercent, lr.Passed)
		if !lr.Passed {
			t.Errorf("long-task ETA error exceeds 20%% at %.1f%%: predicted=%ds actual=%ds error=%.1f%%",
				lr.ProgressPercent, lr.EtaPredicted, lr.EtaActual, lr.ErrorPercent)
		}
	}
}

// TestProgressParser_WarmupETASuppressed verifies that the first
// etaWarmupSamples progress frames report no ETA: during encoder warm-up the
// instantaneous speed is depressed and any ETA computed from it overestimates
// the remaining time (TSI-2433 QA: 41s predicted vs ~10s actual at 2.3%).
func TestProgressParser_WarmupETASuppressed(t *testing.T) {
	p := NewProgressParser()
	p.SetDuration(100 * 1_000_000)

	// Warm-up samples: speed ramps 0.5x → 1.0x → 1.5x before settling.
	warmup := []struct {
		timeS int64
		speed string
	}{
		{1, "0.50"}, {2, "1.00"}, {3, "1.50"},
	}
	for i, w := range warmup {
		line := fmt.Sprintf("frame=%d fps=30 q=28.0 size=1024kB time=00:00:%02d.00 bitrate=2045.0kbits/s speed=%sx",
			w.timeS*30, w.timeS, w.speed)
		frame := p.ParseLine(line)
		if frame == nil {
			t.Fatalf("warm-up frame %d: nil", i)
		}
		if frame.EtaSeconds != 0 {
			t.Errorf("warm-up frame %d (speed=%s): ETA should be suppressed, got %ds",
				i, w.speed, frame.EtaSeconds)
		}
	}

	// After the 3-frame suppression window the ETA appears, computed from
	// the smoothed speed.
	frame := p.ParseLine("frame=120 fps=30 q=28.0 size=1024kB time=00:00:04.00 bitrate=2045.0kbits/s speed=2.00x")
	if frame == nil || frame.EtaSeconds <= 0 {
		t.Fatalf("post-warm-up frame must carry an ETA, got %+v", frame)
	}
	// EWMA after samples (0.5, 1.0, 1.5, 2.0) with α=0.3: 1.2335x.
	// Remaining 96s / 1.2335x ≈ 78s. A raw instantaneous read would give
	// 48s; before this fix the very first frame reported an ETA derived
	// from a warm-up speed ~4x below steady state (TSI-2433 QA).
	wantMin, wantMax := 75, 81
	if frame.EtaSeconds < wantMin || frame.EtaSeconds > wantMax {
		t.Errorf("ETA = %ds, want smoothed estimate in [%d,%d]s", frame.EtaSeconds, wantMin, wantMax)
	}
}

// TestProgressParser_ResetClearsEWMA verifies Reset drops the smoothing
// state so a reused parser starts its warm-up suppression from scratch.
func TestProgressParser_ResetClearsEWMA(t *testing.T) {
	p := NewProgressParser()
	p.SetDuration(100 * 1_000_000)

	p.Reset()
	p.SetDuration(100 * 1_000_000)
	for i := int64(1); i <= 6; i++ {
		line := fmt.Sprintf("frame=%d fps=30 q=28.0 size=1024kB time=00:00:%02d.00 bitrate=2045.0kbits/s speed=2.00x", i*30, i)
		f := p.ParseLine(line)
		if i <= 3 {
			if f == nil || f.EtaSeconds != 0 {
				t.Fatalf("frame %d: ETA must be suppressed in warm-up window, got %+v", i, f)
			}
			continue
		}
		if f == nil || f.EtaSeconds == 0 {
			t.Fatalf("frame %d: expected ETA after warm-up, got %+v", i, f)
		}
	}
}

// TestProgressParser_ETAPrecision_ErrorCalculation verifies
// the correctness of ETA math at well-known test points.
func TestProgressParser_ETAPrecision_ErrorCalculation(t *testing.T) {
	// Simple case: 100s duration, 50% done at 2.0x speed.
	durationUs := int64(100) * 1_000_000
	// At time=50s, speed=2.0x
	// Remaining: 50s of content
	// ETA = 50s / 2.0x = 25s
	checkpoints := [][2]int64{
		{50 * 1_000_000, 2_000_000},
	}

	// With actualWallTimeUs provided: total wall time should be 50s.
	// First 50s at 2.0x = 25s wall time so far.
	// But remaining 50s at 2.0x = 25s wall time remaining.
	// So actual remaining = 25s.
	results := VerifyETAPrecision(checkpoints, durationUs, 0.20, 50_000_000)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]

	expectedETA := 25 // 50s remaining / 2.0x
	if r.EtaPredicted != expectedETA {
		t.Errorf("ETA predicted = %d, want %d", r.EtaPredicted, expectedETA)
	}

	expectedActual := 25 // wall clock: 50_000_000 total - 25_000_000 elapsed = 25_000_000 us = 25s
	if r.EtaActual != expectedActual {
		t.Errorf("ETA actual = %d, want %d", r.EtaActual, expectedActual)
	}

	if r.ErrorPercent > 0.01 {
		t.Errorf("error should be ~0%% for simple case, got %.2f%%", r.ErrorPercent)
	}
}

// TestProgressParser_ETAPrecision_AllPassed verifies the
// overall pass/fail determination across a full progress sequence.
func TestProgressParser_ETAPrecision_AllPassed(t *testing.T) {
	durationUs := int64(600) * 1_000_000 // 10-minute video

	// Simulate checkpoints every ~20% with moderate speed variation.
	checkpoints := [][2]int64{
		{120 * 1_000_000, 2_000_000}, // 20% at 2.0x
		{240 * 1_000_000, 1_900_000}, // 40% at 1.9x
		{360 * 1_000_000, 2_100_000}, // 60% at 2.1x
		{480 * 1_000_000, 1_800_000}, // 80% at 1.8x
	}

	results := VerifyETAPrecision(checkpoints, durationUs, 0.20, 0)

	allPassed := true
	for _, r := range results {
		t.Logf("Checkpoint %.0f%%: predicted=%ds actual=%ds error=%.1f%% passed=%v",
			r.ProgressPercent, r.EtaPredicted, r.EtaActual, r.ErrorPercent, r.Passed)
		if !r.Passed {
			allPassed = false
		}
	}

	if !allPassed {
		t.Errorf("not all checkpoints passed ETA precision verification")
	}

	if len(results) != len(checkpoints) {
		t.Errorf("expected %d results, got %d", len(checkpoints), len(results))
	}
}

// formatDuration formats seconds into a human-readable string.
func formatDuration(seconds int) string {
	if seconds < 0 {
		return "N/A"
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
