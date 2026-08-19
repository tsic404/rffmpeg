package worker

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ProgressFrame represents a parsed progress update from ffmpeg stderr.
type ProgressFrame struct {
	TimeUs      int64   // current decoded time in microseconds
	DurationUs  int64   // total duration in microseconds (0 if unknown)
	Percent     float64 // 0-100, or -1 if duration unknown
	Speed       float64 // speed multiplier (e.g., 2.05 for 2.05x)
	EtaSeconds  int     // estimated seconds remaining (0 if unknown)
	TimeElapsed string  // raw time= string
	RawLine     string  // original stderr line
}

// timeRegex matches ffmpeg time=HH:MM:SS.ms pattern.
var timeRegex = regexp.MustCompile(`time=(\d+):(\d+):(\d+)\.(\d+)`)

// speedRegex matches ffmpeg speed=N.NNx pattern.
var speedRegex = regexp.MustCompile(`speed=\s*([0-9.]+)x`)

// durationRegex matches ffmpeg Duration: HH:MM:SS.ms pattern (from output header).
var durationRegex = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+)\.(\d+)`)

// ProgressParser parses ffmpeg stderr output for progress information.
type ProgressParser struct {
	durationUs      int64
	startTime       time.Time
	lastPushTime    time.Time
	minPushInterval time.Duration
}

// NewProgressParser creates a new progress parser.
func NewProgressParser() *ProgressParser {
	return &ProgressParser{
		minPushInterval: 1 * time.Second, // throttle to 1 push per second
	}
}

// SetDuration sets the known total duration from an external source (e.g., prior probe).
func (p *ProgressParser) SetDuration(durationUs int64) {
	p.durationUs = durationUs
}

// ParseDurationLine attempts to extract Duration from an ffmpeg header line.
// Returns the duration in microseconds, or 0 if not found.
func ParseDurationLine(line string) int64 {
	matches := durationRegex.FindStringSubmatch(line)
	if matches == nil {
		return 0
	}
	return parseTimeToUs(matches[1], matches[2], matches[3], matches[4])
}

// ParseLine parses a single stderr line for time= and speed= information.
// Returns nil if the line contains no progress information.
func (p *ProgressParser) ParseLine(line string) *ProgressFrame {
	timeMatches := timeRegex.FindStringSubmatch(line)
	if timeMatches == nil {
		return nil
	}

	timeUs := parseTimeToUs(timeMatches[1], timeMatches[2], timeMatches[3], timeMatches[4])

	frame := &ProgressFrame{
		TimeUs:      timeUs,
		TimeElapsed: fmt.Sprintf("%s:%s:%s.%s", timeMatches[1], timeMatches[2], timeMatches[3], timeMatches[4]),
		RawLine:     line,
	}

	// Extract speed
	speedMatches := speedRegex.FindStringSubmatch(line)
	if speedMatches != nil {
		if s, err := strconv.ParseFloat(speedMatches[1], 64); err == nil {
			frame.Speed = s
		}
	}

	// Use stored duration if available, otherwise try to parse from line
	durationUs := p.durationUs
	if durationUs == 0 {
		durMatches := durationRegex.FindStringSubmatch(line)
		if durMatches != nil {
			durationUs = parseTimeToUs(durMatches[1], durMatches[2], durMatches[3], durMatches[4])
			p.durationUs = durationUs
		}
	}
	frame.DurationUs = durationUs

	// Calculate percent
	if durationUs > 0 && timeUs >= 0 {
		frame.Percent = float64(timeUs) / float64(durationUs) * 100.0
		if frame.Percent > 100 {
			frame.Percent = 100
		}
	} else {
		frame.Percent = -1 // unknown
	}

	// Calculate ETA
	if durationUs > 0 && timeUs > 0 && frame.Speed > 0 {
		remainingUs := durationUs - timeUs
		if remainingUs < 0 {
			remainingUs = 0
		}
		frame.EtaSeconds = int(float64(remainingUs) / (frame.Speed * 1_000_000))
	}

	return frame
}

// ShouldPush returns true if enough time has elapsed since the last push.
func (p *ProgressParser) ShouldPush() bool {
	now := time.Now()
	if p.lastPushTime.IsZero() || now.Sub(p.lastPushTime) >= p.minPushInterval {
		p.lastPushTime = now
		return true
	}
	return false
}

// Reset resets the parser state for a new job.
func (p *ProgressParser) Reset() {
	p.startTime = time.Now()
	p.lastPushTime = time.Time{}
	p.durationUs = 0
}

// parseTimeToUs converts hh, mm, ss, ms components to microseconds.
func parseTimeToUs(hh, mm, ss, ms string) int64 {
	h, _ := strconv.Atoi(hh)
	m, _ := strconv.Atoi(mm)
	s, _ := strconv.Atoi(ss)
	frac := ms
	// Pad or trim milliseconds to 6 digits for microseconds
	for len(frac) < 6 {
		frac += "0"
	}
	if len(frac) > 6 {
		frac = frac[:6]
	}
	us, _ := strconv.ParseInt(frac, 10, 64)

	return int64(h)*3600_000_000 + int64(m)*60_000_000 + int64(s)*1_000_000 + us
}

// parseSpeed extracts the speed multiplier from an ffmpeg stderr line.
func parseSpeed(line string) float64 {
	matches := speedRegex.FindStringSubmatch(line)
	if matches == nil {
		return 0
	}
	s, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0
	}
	return s
}

// FilterProgressLine returns true if a line contains progress information.
func FilterProgressLine(line string) bool {
	return strings.Contains(line, "time=")
}

// ETAVerificationResult holds the outcome of an ETA precision check at a single checkpoint.
type ETAVerificationResult struct {
	ProgressPercent float64 // percent complete at this checkpoint
	EtaPredicted    int     // ETA predicted by the parser (seconds)
	EtaActual       int     // ground-truth remaining time (seconds)
	ErrorPercent    float64 // absolute relative error in percent
	Passed          bool    // whether error is within the configured tolerance
}

// VerifyETAPrecision simulates ETA predictions for a long task and checks
// that errors at key progress checkpoints stay within tolerance.
//
// checkpoints: pairs of (time microseconds, speed encoded as an integer
// where 1_000_000 = 1.0x). They must be in ascending time order.
//
// durationUs: total media duration in microseconds.
//
// tolerance: maximum acceptable relative error (e.g., 0.20 for 20%).
//
// actualWallTimeUs: optional ground-truth total wall clock time in microseconds.
// When 0, the function infers the wall time from the last checkpoint and assumes
// the remaining portion from the last checkpoint to completion runs at the last
// observed speed (best-effort approximation).
func VerifyETAPrecision(checkpoints [][2]int64, durationUs int64, tolerance float64, actualWallTimeUs int64) []ETAVerificationResult {
	p := NewProgressParser()
	p.SetDuration(durationUs)

	type checkpointMeta struct {
		timeUs int64
		speed  float64
		wallUs int64 // cumulative wall time so far
	}

	var metas []checkpointMeta
	var cumulativeWallUs int64

	// Build cumulative wall time from checkpoints.
	for i, cp := range checkpoints {
		timeUs := cp[0]
		speed := float64(cp[1]) / 1_000_000.0 // convert to float multiplier

		if i == 0 {
			// First segment: wall time = time / speed (in microseconds)
			cumulativeWallUs = int64(float64(timeUs) / speed)
		} else {
			prevTimeUs := metas[i-1].timeUs
			segDurationUs := timeUs - prevTimeUs
			cumulativeWallUs += int64(float64(segDurationUs) / speed)
		}

		metas = append(metas, checkpointMeta{
			timeUs: timeUs,
			speed:  speed,
			wallUs: cumulativeWallUs,
		})
	}

	// Total wall time: if not provided, extrapolate from last checkpoint to duration end.
	totalWallUs := actualWallTimeUs
	if totalWallUs == 0 && len(metas) > 0 {
		last := metas[len(metas)-1]
		remainingUs := durationUs - last.timeUs
		if remainingUs > 0 {
			totalWallUs = last.wallUs + int64(float64(remainingUs)/last.speed)
		} else {
			totalWallUs = last.wallUs
		}
	}

	// Parse each checkpoint line and verify ETA.
	var results []ETAVerificationResult
	for _, m := range metas {
		// Build a synthetic ffmpeg progress line.
		line := buildProgressLine(m.timeUs, m.speed)
		frame := p.ParseLine(line)
		if frame == nil {
			continue
		}

		progressPct := frame.Percent
		etaPredicted := frame.EtaSeconds

		// Ground-truth remaining wall time at this checkpoint.
		actualRemainingUs := totalWallUs - m.wallUs
		if actualRemainingUs < 0 {
			actualRemainingUs = 0
		}
		etaActual := int(actualRemainingUs / 1_000_000)

		var errorPct float64
		var passed bool
		if etaActual > 0 {
			errorPct = float64(absInt(etaPredicted-etaActual)) / float64(etaActual) * 100.0
			passed = errorPct <= tolerance*100
		} else {
			// When actual remaining is 0 (encoding finished), ETA should be 0.
			if etaPredicted == 0 {
				errorPct = 0
				passed = true
			} else {
				errorPct = 100.0
				passed = false
			}
		}

		results = append(results, ETAVerificationResult{
			ProgressPercent: progressPct,
			EtaPredicted:    etaPredicted,
			EtaActual:       etaActual,
			ErrorPercent:    errorPct,
			Passed:          passed,
		})
	}

	return results
}

// buildProgressLine creates a synthetic ffmpeg progress line for testing.
func buildProgressLine(timeUs int64, speed float64) string {
	totalSec := int(timeUs / 1_000_000)
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	ms := (timeUs % 1_000_000) / 10_000 // centiseconds for ffmpeg format
	totalKB := timeUs / 1000            // approximate size
	return fmt.Sprintf("frame=%d fps=30 q=28.0 size=%dkB time=%02d:%02d:%02d.%02d bitrate=2045.0kbits/s speed=%.2fx",
		totalSec*30, totalKB, h, m, s, ms, speed)
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
