package worker

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tsic404/rffmpeg/pkg/ffmpegopts"
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
//
// For trimmed jobs (-ss/-t/-to) the progress must be anchored to the
// output window, not the full input duration: ffmpeg's time= counts from
// zero within the segment, so dividing it by the full input duration
// never reaches 100% and ETA is off by orders of magnitude.
type ProgressParser struct {
	// durationUs is the total duration percent/ETA are measured against.
	// For trimmed jobs this is the output window (min(-t, input-ss) or
	// -to-ss), not the full input duration.
	durationUs      int64
	startTime       time.Time
	lastPushTime    time.Time
	minPushInterval time.Duration
	// nowFunc returns the current time. Overridden in tests to inject
	// deterministic wall-clock progression.
	nowFunc func() time.Time
	// arrived. Wall-clock elapsed since then divided by media time encoded
	// gives the true end-to-end throughput, which naturally absorbs encoder
	// warm-up, I/O, and finalization overhead — the EWMA-speed approach
	// missed all of these (TSI-2438 QA: +37% early, -42% late).
	firstSampleTime time.Time
	// ewmaSpeed is kept for display (the instantaneous speed field on
	// WSProgressPayload) but is no longer used for ETA computation.
	ewmaSpeed float64
	// speedSamples counts speed observations seen since Reset. The first
	// etaWarmupSeconds of wall-clock time suppress ETA: not enough data has
	// accumulated for a stable wall-clock rate.
	speedSamples int
}

// etaSpeedAlpha is the EWMA smoothing factor for speed observations used in
// the speed field of WSProgressPayload (display only, not ETA).
const etaSpeedAlpha = 0.3

// etaMinWallSeconds is the minimum wall-clock seconds that must elapse
// before the parser emits an ETA. Before this, the wall-clock rate is
// unstable (dominated by warm-up) and reporting nothing is better than
// reporting a number known to be wrong.
const etaMinWallSeconds = 3.0

func NewProgressParser() *ProgressParser {
	return &ProgressParser{
		minPushInterval: 1 * time.Second, // throttle to 1 push per second
		nowFunc:         time.Now,
	}
}

// now returns the current time, using the injectable nowFunc if set.
func (p *ProgressParser) now() time.Time {
	if p.nowFunc != nil {
		return p.nowFunc()
	}
	return time.Now()
}

// SetDuration sets the known total duration from an external source (e.g., prior probe).
func (p *ProgressParser) SetDuration(durationUs int64) {
	p.durationUs = durationUs
}

// DurationUs returns the current progress denominator (output window for
// trimmed jobs, full input duration otherwise). Zero means unknown.
func (p *ProgressParser) DurationUs() int64 {
	return p.durationUs
}

// SetSeekWindow narrows the progress denominator to an output window
// derived from -ss/-t/-to so percent and ETA reflect the trimmed segment,
// not the full input. It must be called after SetDuration (which seeded the
// full input duration): if seekUs/tUs is zero or negative the window is
// left unchanged, and an explicit -t/-to that exceeds the input is clamped.
func (p *ProgressParser) SetSeekWindow(inputDurationUs, seekUs, tUs, toUs int64) {
	if inputDurationUs <= 0 {
		return
	}
	windowUs := inputDurationUs
	// -to is an absolute timestamp within the input; convert to a duration.
	if toUs > 0 {
		windowUs = toUs
		if windowUs > inputDurationUs {
			windowUs = inputDurationUs
		}
	}
	if seekUs > 0 {
		if seekUs >= windowUs {
			// -ss beyond the window leaves nothing to encode; set the
			// denominator to 0 so percent reports -1 (unknown) rather than
			// dividing by zero or silently keeping the full input duration.
			windowUs = 0
		} else {
			windowUs -= seekUs
		}
	}
	if tUs > 0 && (windowUs == 0 || tUs < windowUs) {
		windowUs = tUs
	}
	// Always overwrite: a degenerate window (0) must clear the full-input
	// duration that SetDuration seeded, so percent falls back to -1 instead
	// of reporting a meaningless ratio against the full input.
	p.durationUs = windowUs
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

	// Track the instantaneous speed for display (WSProgressPayload.Speed),
	// folded into an EWMA for a stable display value. This is NOT used for
	// ETA — see wall-clock rate below.
	if frame.Speed > 0 {
		p.speedSamples++
		if p.ewmaSpeed == 0 {
			p.ewmaSpeed = frame.Speed
		} else {
			p.ewmaSpeed = etaSpeedAlpha*frame.Speed + (1-etaSpeedAlpha)*p.ewmaSpeed
		}
	}

	// ETA from wall-clock throughput: remaining media time divided by the
	// overall encode rate (media done / wall elapsed). Unlike the old
	// EWMA-speed approach this naturally absorbs encoder warm-up,
	// finalization overhead, and I/O stalls — all of which are invisible to
	// ffmpeg's speed= multiplier but dominate short-to-medium task timing
	// (TSI-2438 QA: +37% early / -42% late with the old formula).
	if durationUs > 0 && timeUs > 0 && frame.Speed > 0 {
		now := p.now()
		if p.firstSampleTime.IsZero() {
			p.firstSampleTime = now
		}
		wallElapsed := now.Sub(p.firstSampleTime)
		if wallElapsed >= time.Duration(etaMinWallSeconds*float64(time.Second)) {
			wallSec := wallElapsed.Seconds()
			mediaSec := float64(timeUs) / 1_000_000.0
			if mediaSec > 0 && wallSec > 0 {
				rate := mediaSec / wallSec // media-seconds per wall-second
				remainingMediaSec := float64(durationUs-timeUs) / 1_000_000.0
				if remainingMediaSec < 0 {
					remainingMediaSec = 0
				}
				frame.EtaSeconds = int(remainingMediaSec / rate)
			}
		}
	}

	return frame
}

// ShouldPush returns true if enough time has elapsed since the last push.
func (p *ProgressParser) ShouldPush() bool {
	now := p.now()
	if p.lastPushTime.IsZero() || now.Sub(p.lastPushTime) >= p.minPushInterval {
		p.lastPushTime = now
		return true
	}
	return false
}

// Reset resets the parser state for a new job.
func (p *ProgressParser) Reset() {
	p.startTime = p.now()
	p.lastPushTime = time.Time{}
	p.durationUs = 0
	p.firstSampleTime = time.Time{}
	p.ewmaSpeed = 0
	p.speedSamples = 0
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

// parseFFmpegDurationUs parses an ffmpeg duration specification (seconds as a
// decimal number, or HH:MM:SS[.ms] timecode) into microseconds. Returns 0
// on any parse failure or non-positive value.
func parseFFmpegDurationUs(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Timecode form: HH:MM:SS[.fraction]
	if strings.Contains(s, ":") {
		parts := strings.SplitN(s, ":", 3)
		if len(parts) < 2 {
			return 0
		}
		var hh, mm int
		var rest string
		switch len(parts) {
		case 2:
			mm, _ = strconv.Atoi(parts[0])
			rest = parts[1]
		case 3:
			hh, _ = strconv.Atoi(parts[0])
			mm, _ = strconv.Atoi(parts[1])
			rest = parts[2]
		}
		// rest = SS[.fraction]; an empty rest (trailing colon) is invalid.
		if rest == "" {
			return 0
		}
		var ss int
		var frac string
		if dot := strings.IndexByte(rest, '.'); dot != -1 {
			ss, _ = strconv.Atoi(rest[:dot])
			frac = rest[dot+1:]
		} else {
			ss, _ = strconv.Atoi(rest)
		}
		for len(frac) < 6 {
			frac += "0"
		}
		if len(frac) > 6 {
			frac = frac[:6]
		}
		us, _ := strconv.ParseInt(frac, 10, 64)
		return int64(hh)*3600_000_000 + int64(mm)*60_000_000 + int64(ss)*1_000_000 + us
	}
	// Plain seconds (decimal): 30, 30.5, 1.25
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return int64(secs * 1_000_000)
}

// SeekWindow captures the -ss/-t/-to values extracted from ffmpeg args.
// Each field is in microseconds; a zero value means the option was absent.
type SeekWindow struct {
	SeekUs int64 // -ss start offset
	Tus    int64 // -t output duration
	ToUs   int64 // -to absolute end timestamp
}

// ParseSeekWindow scans ffmpeg-style args for output-side -ss/-t/-to values
// and returns them in microseconds. An option may appear as "-ss value",
// "-ss=value", or "-ss:value" (stream-specifier form). Only output-side
// options (those after the last -i) affect the progress window — input-side
// -ss (before -i) is a demuxer hint and is ignored.
//
// The args slice is the full ffmpeg command line (including -y, -i, etc.)
// as the worker passes it to ffmpeg. Boolean/arity decisions use the
// generated ffmpegopts table so -ss/-t/-to are never mistaken for switches.
func ParseSeekWindow(args []string) SeekWindow {
	var sw SeekWindow
	// Find the index after the last -i (output section starts there).
	// Anything before the last -i is an input/global option; -ss there is a
	// demuxer seek hint, not an output trim, and must not narrow the
	// progress window. Only the exact token "-i" declares an input —
	// "-i"-prefixed options like -init_hw_device, -ignore_unknown, and
	// -id3v2_version are legitimate output-section options.
	outputStart := 0
	skipNext := false
	for i := range len(args) {
		if skipNext {
			skipNext = false
			continue
		}
		arg := args[i]
		if arg == "-i" {
			// -i and its value (next arg) belong to the input section.
			outputStart = i + 2
			skipNext = true
			continue
		}
		// Value-taking option before the last -i: its value is part of the
		// input section, so advance outputStart past the value. This
		// prevents the value (e.g. a filename) from being mistaken for an
		// output option later. Boolean flags and inline "=value" forms
		// don't consume a separate value arg.
		if i < outputStart && !ffmpegopts.IsBoolean(arg) && !strings.Contains(arg, "=") && i+1 < len(args) {
			outputStart = i + 2
			skipNext = true
		}
	}
	if outputStart > len(args) {
		outputStart = len(args)
	}

	// Walk the output section; for each target flag, grab its value from the
	// next arg or from an inline = / : form.
	for i := outputStart; i < len(args); i++ {
		arg := args[i]
		// Strip leading "-" or "--".
		name := strings.TrimLeft(arg, "-")
		if name == "" {
			continue
		}
		// Split stream specifier: "-ss" / "-ss:0" → base "ss".
		base := name
		if idx := strings.IndexByte(name, ':'); idx != -1 {
			base = name[:idx]
		}
		// Inline = value: "-ss=10" / "-ss:0=10".
		var inlineVal string
		var hasInline bool
		if eq := strings.IndexByte(name, '='); eq != -1 {
			inlineVal = name[eq+1:]
			hasInline = true
			// Re-derive base from before the stream specifier or =.
			beforeEq := name[:eq]
			if col := strings.IndexByte(beforeEq, ':'); col != -1 {
				base = beforeEq[:col]
			} else {
				base = beforeEq
			}
		}

		switch base {
		case "ss", "t", "to":
			val := ""
			if hasInline {
				val = inlineVal
			} else if i+1 < len(args) {
				val = args[i+1]
				i++ // consume the value
			}
			us := parseFFmpegDurationUs(val)
			switch base {
			case "ss":
				sw.SeekUs = us
			case "t":
				sw.Tus = us
			case "to":
				sw.ToUs = us
			}
		}
	}
	return sw
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

	// Inject wall-clock time via nowFunc so the wall-clock-rate ETA has
	// deterministic data. Feed a priming sample at t=0 (which sets
	// firstSampleTime), then each real checkpoint at t = cumulativeWallUs
	// so wallElapsed = cumulativeWallUs — enough to pass etaMinWallSeconds
	// for all but the very first checkpoints (which are expected to suppress).
	// Inject wall-clock time via nowFunc so the wall-clock-rate ETA has
	// deterministic data. A priming sample at t=0 sets firstSampleTime,
	// then each real checkpoint i is fed at t = cumulativeWallUs[i] so
	// wallElapsed = cumulativeWallUs[i].
	baseTime := time.Unix(0, 0)
	var cpIdx int // index of the next checkpoint to feed (0 = priming)
	p.nowFunc = func() time.Time {
		if cpIdx == 0 {
			// Priming call: firstSampleTime = baseTime (t=0).
			cpIdx++
			return baseTime
		}
		// Real checkpoint cpIdx-1.
		idx := cpIdx - 1
		cpIdx++
		if idx < len(metas) {
			return baseTime.Add(time.Duration(metas[idx].wallUs) * time.Microsecond)
		}
		return baseTime.Add(time.Duration(totalWallUs) * time.Microsecond)
	}

	// Prime: feed an initial sample at t=0 with 1s of media progress so
	// firstSampleTime is set (the timeUs > 0 guard requires non-zero).
	// The 1s media value is negligible vs real checkpoint times.
	if len(metas) > 0 {
		p.ParseLine(buildProgressLine(1_000_000, metas[0].speed))
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
