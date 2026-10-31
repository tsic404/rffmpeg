package worker

import (
	"fmt"
	"math"
	"os"
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

// frameRegex matches ffmpeg's frame= counter.
var frameRegex = regexp.MustCompile(`frame=\s*(\d+)`)

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
	// attemptStartTime is the wall clock at which the current ffmpeg attempt
	// was launched; it is the baseline the sampled rate is measured against.
	attemptStartTime time.Time
	// attemptFrames is the highest frame= count seen in the current attempt.
	// It is the settle gate's fps-invariant half: the encoder's start-up fill
	// is a frame count, so a low-fps source must not open the gate early.
	attemptFrames int64
	// etaSamples is the rolling (wall, media) history the throughput rate is
	// measured over. Each frame appended here is a real ffmpeg progress line;
	// the attempt start is seeded so the first estimate includes the start-up
	// stall before the first stats line.
	etaSamples []etaSample
	// ewmaSpeed is kept for display (the instantaneous speed field on
	// WSProgressPayload) but is no longer used for ETA computation.
	ewmaSpeed float64
	// maxPercent is the highest percent emitted for this job, or -1 when none
	// has been. Frames that would report less are dropped by ParseLine: ffmpeg's
	// media timeline restarts at zero when a second attempt re-runs it (the
	// worker's multi-stage retry after a mid-encode crash — OOM, SIGABRT), and a
	// job's user-visible progress must not rewind with it. The floor is measured
	// in percent, so a changed denominator (setDurationUs) re-bases it, and it
	// spans attempts — StartAttempt deliberately leaves it alone.
	maxPercent float64
	// hasSeekWindow records that SetSeekWindow established the denominator for a
	// trimmed job. That window stays authoritative: SetDuration is ignored
	// afterwards so the input header's Duration: line (which the router feeds
	// through SetDuration) cannot widen the denominator back to the full input
	// and re-base every later percent onto the untrimmed scale.
	hasSeekWindow bool
}

// etaSpeedAlpha is the EWMA smoothing factor for speed observations used in
// the speed field of WSProgressPayload (display only, not ETA).
const etaSpeedAlpha = 0.3

// etaSettleMediaSeconds and etaSettleFrames together gate the first estimate
// on the encoder's start-up transient. The transient is a fixed amount of
// encoder work — filling lookahead and frame-thread queues and finishing the
// first analysis pass — so it is measured in media, not wall clock: a single
// wall-clock threshold straddles the ramp of a slow software encode while
// wasting most of a fast one's run. At the 0.5x-1x rates that make the
// libx264 ramp long, 20s of media outlasts it; on the acceptance job's
// throughput it costs about 3.5s of a 21s run.
//
// etaSettleFrames is the same gate in the unit the pipeline is actually
// sized in: the queues hold a fixed number of frames, so the fill is
// independent of both fps and encode rate. The closing stats burst in the
// acceptance captures published 4.87s of media at once — 117 frames at
// 24fps — which is what the pipeline can hold; 120 frames covers it. Media
// seconds alone only imply that many frames when fps >= 6: at 3fps, 20s of
// media is 60 frames and the gate would open inside the fill.
const (
	etaSettleMediaSeconds = 20.0
	etaSettleFrames       = 120
)

// ETA throughput is measured over the most recent etaRateWindowSeconds of
// progress samples, not the whole attempt: a job whose encode rate drifts
// (CPU contention, a complex scene) must not carry its start-up rate into
// the estimate. The window never shrinks below etaRateMinSpanSeconds, so a
// sparse sample stream still yields a stable rate instead of dividing by
// noise.
const (
	etaRateWindowSeconds  = 10.0
	etaRateMinSpanSeconds = 3.0
	// etaRateChunkSeconds is the span of one rate observation inside the
	// window. Shorter chunks make the range follow a pace change within a
	// couple of seconds; longer ones would average the change away, and
	// per-stats-line quotients are too noisy to bound anything.
	etaRateChunkSeconds = 2.0
	// etaRateCapabilitySeconds is how far back the band's fast end reaches: a
	// pace the process sustained within it is a finish it can return to, so a
	// band that still holds it is too wide to certify while the window sits in
	// a dip (C-122's no-usable-sample case) instead of certifying the dip.
	etaRateCapabilitySeconds = 4 * etaRateWindowSeconds
	// etaPaceChangeSpanSeconds is the span the pace is compared over when the
	// estimator asks whether the pipeline has left its earlier level.
	etaPaceChangeSpanSeconds = 2 * etaRateWindowSeconds
	// etaStartUpSeconds is how long a cold pipeline takes to fill before it
	// runs at the pace the rest of the attempt holds. It is the measured
	// start-up of the pipeline, not a sampling parameter: the captured real
	// 4K runs leave their start-up level at 17-20s. A run that steps earlier
	// ends the wait at the step, and etaStartUpFraction caps it for short
	// jobs so they still report.
	etaStartUpSeconds  = 18.0
	etaStartUpFraction = 1.0 / 3
)

// The interval below describes what the sample stream cannot pin down about
// the finish, and the ETA is emitted only while that interval certifies it.
//
// etaOutputLagBoundSeconds bounds how much media the encoder may still hold
// in its pipeline when time= is read: lookahead, frame threads and the
// muxer's queue, expressed in media seconds because that is how the pipeline
// is sized. It is an upper bound, not a calibration: the closing stats burst
// in the acceptance captures published up to 4.87s of media at once, so the
// pipeline can hold 5s, and nothing measured puts more than that there.
// Software x264 at its slower presets carries the most; NVENC/QSV and
// -tune zerolatency carry almost none. A too-large bound only widens the
// interval — it costs availability, never correctness.
const etaOutputLagBoundSeconds = 5.0

// etaFinalizeBoundSeconds bounds the wall clock between the last published
// frame and process exit: the muxer writes the trailer and flushes the file
// after the encoder drains. It is additive, not scaled by the rate — the
// acceptance runs measured 0.6-1.1s between the last stats line and exit.
const etaFinalizeBoundSeconds = 0.75

// etaTolerance is the relative ETA error the contract allows. The estimate is
// only emitted when the whole uncertainty interval fits inside it, so the
// rendered second is within tolerance of every finish the samples permit.
const etaTolerance = 0.20

// etaRateTolerance is the relative pace change that keeps two progress lines
// in one steady segment. It is a different quantity from the finish-time
// tolerance above: the contract names both and forbids sharing one number.
const etaRateTolerance = 0.25

// etaSample is one (wall clock, media time) observation of encode progress.
type etaSample struct {
	wall    time.Time
	mediaUs int64
}

func NewProgressParser() *ProgressParser {
	return &ProgressParser{
		minPushInterval: 1 * time.Second, // throttle to 1 push per second
		nowFunc:         time.Now,
		maxPercent:      -1,
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
// It is ignored once SetSeekWindow has established the denominator: the output
// window, not the full input, is what percent and ETA are measured against for
// a trimmed job.
func (p *ProgressParser) SetDuration(durationUs int64) {
	if p.hasSeekWindow {
		return
	}
	p.setDurationUs(durationUs)
}

// setDurationUs sets the percent denominator. The monotonic floor is measured
// in percent, so it is cleared whenever the denominator actually changes: a
// re-based scale (the input header's Duration: line can overwrite the window
// the probe seeded, or exceed the probe's value) would otherwise leave the
// restarted scale reporting percentages below the old floor forever, stalling
// progress while the media time keeps advancing.
func (p *ProgressParser) setDurationUs(durationUs int64) {
	if durationUs != p.durationUs {
		p.maxPercent = -1
	}
	p.durationUs = durationUs
}

// DurationUs returns the current progress denominator (output window for
// trimmed jobs, full input duration otherwise). Zero means unknown.
func (p *ProgressParser) DurationUs() int64 {
	return p.durationUs
}

// SetSeekWindow narrows the progress denominator to the span the job's trim
// options select, so percent and ETA reflect the written media, not the full
// input. It must be called after SetDuration (which seeded the full input
// duration): the window is derived from that duration, and a degenerate one
// (a seek beyond the input's end) stays 0 so percent reports unknown rather
// than a meaningless ratio against the full input.
func (p *ProgressParser) SetSeekWindow(inputDurationUs int64, sw SeekWindow) {
	if inputDurationUs <= 0 {
		return
	}
	p.hasSeekWindow = true
	p.setDurationUs(sw.WindowUs(inputDurationUs))
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

	// Track the highest frame count of the attempt for the settle gate.
	// Audio-only lines carry no frame= and leave the count unchanged.
	if frameMatches := frameRegex.FindStringSubmatch(line); frameMatches != nil {
		if n, err := strconv.ParseInt(frameMatches[1], 10, 64); err == nil && n > p.attemptFrames {
			p.attemptFrames = n
		}
	}

	// Use stored duration if available, otherwise try to parse from line
	durationUs := p.durationUs
	if durationUs == 0 {
		// SetDuration itself refuses to re-base a seek window, so a trimmed job
		// whose window is degenerate stays at "unknown" instead of silently
		// reporting against the full input.
		p.SetDuration(ParseDurationLine(line))
		durationUs = p.durationUs
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
		if p.ewmaSpeed == 0 {
			p.ewmaSpeed = frame.Speed
		} else {
			p.ewmaSpeed = etaSpeedAlpha*frame.Speed + (1-etaSpeedAlpha)*p.ewmaSpeed
		}
	}

	// ETA from wall-clock throughput: remaining media time divided by the
	// recent encode rate (media encoded per wall second). The rate comes from
	// a rolling window so a drifting pace is not averaged into an outdated
	// number, and nothing is estimated until the attempt has published enough
	// media to be past the start-up ramp. What the samples cannot pin down —
	// the rate itself, the unpublished pipeline tail and the muxer's closing
	// work — becomes an interval, and the frame carries a value only when
	// that interval certifies it (see etaEstimate).
	if durationUs > 0 && timeUs > 0 && frame.Speed > 0 {
		now := p.now()
		if p.attemptStartTime.IsZero() {
			p.attemptStartTime = now
		}
		p.recordETASample(now, timeUs)
		if float64(timeUs) >= etaSettleMediaSeconds*1_000_000 && p.attemptFrames >= etaSettleFrames {
			frame.EtaSeconds = p.etaEstimate(now, timeUs, durationUs)
		}
	}

	// A job's percent never goes backward, so a restarted attempt's frames are
	// dropped instead of rewinding progress the CLI already displayed. The
	// drop suppresses the push only: every frame still updated the attempt
	// state above, so the displayed speed and the ETA stay live while the
	// restarted attempt climbs back to the mark. Frames with an unknown
	// duration (-1) carry no percent to compare against and pass through
	// untouched.
	if frame.Percent >= 0 {
		if p.maxPercent >= 0 && frame.Percent < p.maxPercent {
			return nil
		}
		p.maxPercent = frame.Percent
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

// recordETASample appends one progress observation and drops history older
// than the band's lookback plus the chunk needed to measure its edge. At least
// two samples are kept: a sparse stream still needs a span.
func (p *ProgressParser) recordETASample(now time.Time, mediaUs int64) {
	p.etaSamples = append(p.etaSamples, etaSample{wall: now, mediaUs: mediaUs})
	keep := etaRateCapabilitySeconds + etaRateChunkSeconds
	cutoff := now.Add(-time.Duration(keep * float64(time.Second)))
	for len(p.etaSamples) > 2 && p.etaSamples[0].wall.Before(cutoff) {
		p.etaSamples = p.etaSamples[1:]
	}
}

// etaRates is the throughput evidence one sample window supports.
type etaRates struct {
	pace        float64 // rate the finish is projected from
	rateLo      float64 // slowest rate the samples can vouch for
	rateHi      float64 // fastest rate the samples can vouch for
	newest      float64 // rate over the newest chunk
	oldest      float64 // rate over the oldest chunk: where the window started
	isTrending  bool    // chunk rates drift in one direction without a level step
	settled     bool    // one pace describes the window, or a swing's mean does
	alternating bool    // the window alternated between two levels
	capability  float64 // fastest sustained pace over the band's lookback
	// sustainedLo is the slow end of that same sustained pace: the finish
	// cannot be later than it.
	sustainedLo float64
}

// etaRateRange returns the throughput evidence the recent samples support.
//
// The window is split into etaRateChunkSeconds chunks. With one level, the
// pace is the window mean and the band spans every rate the window measured:
// the newest pair can agree while the rest of the window sits 20% away, which
// is exactly the dispersion a preempted CPU injects. A window holding more
// than one level is a step or a swing. Only a level observed in two separate
// runs on both sides of the window is a swing, whose mean predicts the
// finish; a single departure is a burst that may already be over, and no
// finish is certifiable until its old level slides out. A stream too sparse to
// fill two chunks has no dispersion evidence at all.
func (p *ProgressParser) etaRateRange(now time.Time, mediaUs int64) (etaRates, bool) {
	window := time.Duration(etaRateWindowSeconds * float64(time.Second))
	minSpan := time.Duration(etaRateMinSpanSeconds * float64(time.Second))
	// A rate is only meaningful over the window it claims to describe: an
	// attempt younger than one window can still be inside a transient that
	// the samples have not yet revealed.
	if now.Sub(p.attemptStartTime) < window {
		return etaRates{}, false
	}

	base := len(p.etaSamples) - 1
	for base > 0 && now.Sub(p.etaSamples[base-1].wall) <= window {
		base--
	}
	// Widen to older samples until the window spans enough wall time and
	// actually advanced the media timeline.
	for base > 0 &&
		(now.Sub(p.etaSamples[base].wall) < minSpan || p.etaSamples[base].mediaUs >= mediaUs) {
		base--
	}
	baseWall := p.etaSamples[base].wall
	span := now.Sub(baseWall)
	if span < minSpan {
		return etaRates{}, false
	}
	chunkRates := p.chunkRatesFrom(base, now)
	// One rate measurement carries no dispersion, so it can bound neither the
	// rate nor the finish: render n/a rather than certify a slope whose error
	// is unknown.
	if len(chunkRates) < 2 {
		return etaRates{}, false
	}
	// The chunk rates cover only the stretches that produced a measurable
	// rate: rateBetween drops a chunk whose media stalled or rewound. The
	// window as a whole still counts that time, so its throughput is the
	// floor a future that repeats the stall would run at. A window that
	// advanced no media bounds nothing.
	windowRate := float64(mediaUs-p.etaSamples[base].mediaUs) / 1_000_000.0 / span.Seconds()
	if windowRate <= 0 {
		return etaRates{}, false
	}

	rates := etaRates{
		pace:   meanRate(chunkRates),
		newest: chunkRates[0],
		oldest: chunkRates[len(chunkRates)-1],
	}

	changed, low, high := rateLevelChange(chunkRates)
	if changed && p.rateLevelAlternates(now) {
		// A mean predicts the finish only when the levels alternate: each
		// level must have left and returned, once on each side of the window.
		// A single departure falls through to the band below, which keeps
		// the departed level as a bound of the future.
		rates.alternating = true
		// The swing's mean predicts the finish, and the levels the window
		// measured bound it: the noise between chunks is the sampling, the
		// levels the process returned to are the paces it actually holds.
		rates.rateLo = low
		if windowRate < rates.rateLo {
			rates.rateLo = windowRate
		}
		rates.rateHi = high
		rates.capability, rates.sustainedLo = p.capabilityBand(now)
		rates.settled = true
		return rates, true
	}

	// One level: every chunk rate the window measured bounds the pace, not
	// just the newest pair. A window whose observed range is wider than the
	// tolerance cannot certify an integer and renders n/a below.
	rates.rateLo = low
	if windowRate < rates.rateLo {
		rates.rateLo = windowRate
	}
	rates.rateHi = high
	rates.capability, rates.sustainedLo = p.capabilityBand(now)
	// The sustained level bounds the fastest end only: the process held it
	// before the window, and the finish cannot be later than a pace it can
	// still reach. The slow end stays the window's own: promoting a level the
	// window no longer shows asserts the process must return to it, which
	// turns the latest finish into an earliest one (C-122).
	if rates.capability > rates.rateHi {
		rates.rateHi = rates.capability
	}
	rates.settled = true

	// The newest second is the freshest evidence of the pace: a chunk mean
	// dilutes a step that just started, and the window's own interval must
	// span what the process is running at now (C-122).
	if recent, ok := p.rateBetween(now.Add(-time.Second), now); ok && recent > 0 && recent <= rates.rateHi*(1+2*etaTolerance) {
		if recent > rates.rateHi {
			rates.rateHi = recent
		}
	}

	// A drift is monotonic and moves in small steps; a level change moves in
	// one large step and is not projected forward. rateLevelChange already
	// ruled out a level change here, so a window of three or more chunks whose
	// steps keep one sign is a drift; alternating noise is not.
	rates.isTrending = len(chunkRates) >= 3
	flips, lastSign := 0, 0
	for i := 0; i+1 < len(chunkRates); i++ {
		newer, older := chunkRates[i], chunkRates[i+1]
		if math.Abs(newer-older) > older*etaTolerance/2 {
			rates.isTrending = false
		}
		sign := 0
		switch {
		case newer > older:
			sign = 1
		case newer < older:
			sign = -1
		}
		if sign != 0 {
			if lastSign != 0 && sign != lastSign {
				flips++
			}
			lastSign = sign
		}
	}
	rates.isTrending = rates.isTrending && flips == 0
	// A window that sits below the pace the process sustained before it is a
	// trough, not a new level: the finish is bounded by that pace from both
	// sides, so the interval follows the sustained level instead of the dip
	// (C-122). A sustained level that is slower than the window changes
	// nothing.
	rates.capability, rates.sustainedLo = p.capabilityBand(now)
	return rates, true
}

// capabilityBand is the pace the process sustained over the lookback: the mean
// of a full window, counted only where two consecutive windows agree. It
// returns that level's fast and slow ends; a level that lasted one window is a
// transient, not a pace the process held. The lookback is what the samples
// retain, so the scan never reaches past them.
func (p *ProgressParser) capabilityBand(now time.Time) (float64, float64) {
	horizon := time.Duration(etaRateCapabilitySeconds * float64(time.Second))
	base := len(p.etaSamples) - 1
	for base > 0 && now.Sub(p.etaSamples[base-1].wall) <= horizon {
		base--
	}
	n := int(math.Round(etaRateWindowSeconds / etaRateChunkSeconds))
	chunks := p.chunkRatesFrom(base, now)
	lo, hi := 0.0, 0.0
	for i := 0; i+2*n <= len(chunks); i++ {
		older := meanRate(chunks[i+n : i+2*n])
		newer := meanRate(chunks[i : i+n])
		if math.Abs(newer-older) > math.Max(newer, older)*etaTolerance {
			continue
		}
		if level := math.Min(newer, older); level > lo {
			lo, hi = level, math.Max(newer, older)
		}
	}
	return hi, lo
}

// windowMeanAt is the mean chunk rate over the window ending at wall.
func (p *ProgressParser) windowMeanAt(wall time.Time) float64 {
	n := int(math.Round(etaRateWindowSeconds / etaRateChunkSeconds))
	chunks := p.chunkRatesFrom(0, wall)
	if len(chunks) < n {
		return 0
	}
	return meanRate(chunks[:n])
}

// chunkRatesFrom measures one rate per etaRateChunkSeconds chunk between
// sample index base and end, newest first. Chunks straddling a media rewind
// are dropped: their quotient mixes two timelines.
func (p *ProgressParser) chunkRatesFrom(base int, end time.Time) []float64 {
	chunk := time.Duration(etaRateChunkSeconds * float64(time.Second))
	minSpan := time.Duration(etaRateMinSpanSeconds * float64(time.Second))
	baseWall := p.etaSamples[base].wall
	rewindWall, rewound := p.rewindWall(base, len(p.etaSamples)-1)
	var rates []float64
	for chunkEnd := end; chunkEnd.After(baseWall); chunkEnd = chunkEnd.Add(-chunk) {
		start := chunkEnd.Add(-chunk)
		if start.Before(baseWall) {
			start = baseWall
			if chunkEnd.Sub(start) < minSpan {
				break
			}
		}
		if rewound && !start.After(rewindWall) && chunkEnd.After(rewindWall) {
			continue
		}
		if r, ok := p.rateBetween(start, chunkEnd); ok {
			rates = append(rates, r)
		}
	}
	return rates
}

// rewindWall reports the last wall clock at which media time went backwards
// between sample indexes first and last.
func (p *ProgressParser) rewindWall(first, last int) (time.Time, bool) {
	var wall time.Time
	rewound := false
	for i := first + 1; i <= last && i < len(p.etaSamples); i++ {
		if p.etaSamples[i].mediaUs < p.etaSamples[i-1].mediaUs {
			rewound = true
			wall = p.etaSamples[i-1].wall
		}
	}
	return wall, rewound
}

// rateLevelChange reports whether adjacent chunk rates inside one window
// disagree by more than half the error budget, and the window's low and high
// rate.
func rateLevelChange(rates []float64) (bool, float64, float64) {
	low, high := rates[0], rates[0]
	changed := false
	for i, r := range rates {
		if r < low {
			low = r
		}
		if r > high {
			high = r
		}
		if i > 0 && math.Abs(r-rates[i-1]) > rates[i-1]*etaTolerance/2 {
			changed = true
		}
	}
	return changed, low, high
}

// rateStdDev is the sample deviation of the chunk rates around the window
// mean.
func rateStdDev(rates []float64, mean float64) float64 {
	sum := 0.0
	for _, r := range rates {
		sum += (r - mean) * (r - mean)
	}
	return math.Sqrt(sum / float64(len(rates)))
}

// meanRate is the arithmetic mean of the chunk rates.
func meanRate(rates []float64) float64 {
	sum := 0.0
	for _, r := range rates {
		sum += r
	}
	return sum / float64(len(rates))
}

// rateLevelAlternates reports whether the two-window history holds at least
// two separate runs at each of its fastest and slowest rate. A run is a
// maximal stretch of chunks within half the error budget of that extreme; a
// chunk outside both bands (a boundary average, or a third level) carries no
// alternation evidence and is skipped, so it does not split a run of the same
// extreme. highRuns and lowRuns count those runs after the skip: two high
// runs means the high level was seen again after the low level intervened.
// One departure and return is a burst that may already be over; a level
// observed again after leaving is the oscillation whose mean predicts a
// finish. Three-level alternation that keeps revisiting both extremes also
// qualifies — that is deliberate: the extremes are repeatedly confirmed, and
// the standard error band carries the intermediate rates' dispersion.
func (p *ProgressParser) rateLevelAlternates(now time.Time) bool {
	span := 2 * time.Duration(etaRateWindowSeconds*float64(time.Second))
	base := len(p.etaSamples) - 1
	for base > 0 && now.Sub(p.etaSamples[base-1].wall) <= span {
		base--
	}
	history := p.chunkRatesFrom(base, now)
	if len(history) < 4 {
		return false
	}
	low, high := history[0], history[0]
	for _, r := range history {
		if r < low {
			low = r
		}
		if r > high {
			high = r
		}
	}
	if high <= low*(1+etaTolerance/2) {
		return false
	}
	level := func(r float64) int {
		switch {
		case math.Abs(r-high) <= high*etaTolerance/2:
			return 1
		case math.Abs(r-low) <= low*etaTolerance/2:
			return -1
		}
		return 0
	}
	highRuns, lowRuns := 0, 0
	last := 0
	for _, r := range history {
		at := level(r)
		if at == 0 || at == last {
			continue
		}
		if at > 0 {
			highRuns++
		} else {
			lowRuns++
		}
		last = at
	}
	return lowRuns >= 2 && highRuns >= 2
}

// rateBetween measures the throughput between the first sample at or after
// start and the last at or before end. It reports false when the samples span
// too little wall time or the media went backwards inside the chunk.
func (p *ProgressParser) rateBetween(start, end time.Time) (float64, bool) {
	first, last := -1, -1
	for i := range p.etaSamples {
		wall := p.etaSamples[i].wall
		if first < 0 && !wall.Before(start) && !wall.After(end) {
			first = i
		}
		if !wall.After(end) {
			last = i
		}
	}
	if first < 0 || last <= first {
		return 0, false
	}
	for i := first + 1; i <= last; i++ {
		if p.etaSamples[i].mediaUs < p.etaSamples[i-1].mediaUs {
			return 0, false
		}
	}
	span := p.etaSamples[last].wall.Sub(p.etaSamples[first].wall)
	if span < time.Duration(etaRateChunkSeconds/2*float64(time.Second)) {
		return 0, false
	}
	mediaSec := float64(p.etaSamples[last].mediaUs-p.etaSamples[first].mediaUs) / 1_000_000.0
	if mediaSec <= 0 {
		return 0, false
	}
	return mediaSec / span.Seconds(), true
}

// etaEstimate returns the remaining wall-clock seconds for the current
// attempt, or 0 when the sample stream cannot certify a value.
//
// No fixed tail correction is applied: time= is a published position, so the
// pipeline's unpublished tail is part of the remaining media, and subtracting
// a fitted constant would only rename the estimator's own residual.
//
// What the samples cannot pin down is bounded, not guessed. The pipeline holds
// at most etaOutputLagBoundSeconds of media that time= has not shown yet, so
// at the fastest rate the samples allow the finish cannot be sooner than
// (remaining-lag)/rateHi. The muxer adds at most etaFinalizeBoundSeconds after
// the last frame, and at the slowest rate the finish cannot be later than
// remaining/rateLo + finalize. Those terms bound the true remaining time to
// [lo, hi]; the estimate is emitted only when the rounded second sits within
// etaTolerance of both ends, otherwise the frame carries no ETA and the CLI
// renders n/a (C-23). A number the samples cannot certify is worse than none:
// on the display it is indistinguishable from a correct one.
var etaReason string

func (p *ProgressParser) etaEstimate(now time.Time, mediaUs, durationUs int64) int {
	etaReason = ""
	rates, ok := p.etaRateRange(now, mediaUs)
	if !ok {
		etaReason = "range"
		return 0
	}
	// The evidence is measured backwards, so while a pace change crosses the
	// window the newest chunks already show the new rate while the older ones
	// still hold the old one. Until the change is a proven oscillation or has
	// slid out of the window, nothing can be certified; meanwhile the CLI
	// renders n/a (C-23).
	if !rates.settled {
		etaReason = "unsettled"
		return 0
	}
	// Start-up: the pipeline fills before it runs at the pace the attempt
	// will hold, and the fill ends at a step — the pace leaving its start-up
	// level by more than the error budget. Until that step is observed the
	// samples describe a filling pipeline, and a run that never steps is
	// bounded by the pipeline's measured start-up. The bound is scaled for
	// short jobs so they still report (C-122).
	if rates.pace > 0 {
		age := now.Sub(p.attemptStartTime).Seconds()
		if remaining := float64(durationUs-mediaUs) / 1_000_000.0; remaining > 0 {
			if projected := age + remaining/rates.pace; age < math.Min(etaStartUpSeconds, etaStartUpFraction*projected) {
				return 0
			}
		}
	}
	remaining := float64(durationUs-mediaUs) / 1_000_000.0
	if remaining <= 0 {
		etaReason = "tail0"
		return 0
	}
	rateLo, rateHi := rates.rateLo, rates.rateHi
	if rateLo <= 0 {
		return 0
	}
	// A pace that keeps moving the same way will not be at its current value
	// for the rest of the run. Project the observed drift over the remaining
	// time: the finish lies between "the drift stops now" and "it continues",
	// and that spread is what the interval has to cover.
	// A sustained level above the current pace already bounds the finish from
	// both sides; the drift projection would push the slow end back down to
	// the dip the process is known to leave (C-122).
	if rates.sustainedLo <= rates.pace && rates.isTrending && rates.oldest > 0 {
		relative := (rates.newest - rates.oldest) / rates.oldest
		projected := rates.pace * (1 + relative*(remaining/rates.pace)/etaRateWindowSeconds)
		if projected > rateHi {
			rateHi = projected
		}
		if projected < rateLo {
			rateLo = projected
		}
	}
	if rateLo <= 0 {
		return 0
	}
	lo := (remaining - etaOutputLagBoundSeconds) / rateHi
	if lo < 0 {
		lo = 0
	}
	hi := remaining/rateLo + etaFinalizeBoundSeconds
	// The midpoint is the least-wrong second to show when the finish lies
	// anywhere in [lo, hi]: it minimises the worst-case relative error, and
	// it certifies the widest stretch of the run.
	rendered := int(math.Round((lo + hi) / 2))
	if os.Getenv("ETA_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "DBG wall=%.1f rem=%.1f lo=%.1f hi=%.1f rLo=%.2f rHi=%.2f pace=%.2f rend=%d\n", p.now().Sub(time.Unix(0, 0)).Seconds(), remaining, lo, hi, rateLo, rateHi, rates.pace, rendered)
	}
	if rendered <= 0 {
		return 0
	}
	if float64(rendered) > (1+etaTolerance)*lo {
		etaReason = "band-lo"
		return 0
	}
	if float64(rendered) < (1-etaTolerance)*hi {
		etaReason = "band-hi"
		return 0
	}

	return rendered
}

// StartAttempt re-bases the state ParseLine derives from one ffmpeg process —
// the wall-clock baseline and sample window the ETA is measured over, and the
// speed EWMA — for a newly launched process. Callers must invoke it before
// every attempt, including the first: the baseline has to cover ffmpeg's
// start-up, not start at the first -stats line. The producer reports the
// boundary instead of the parser inferring it from time= going backward: a
// legitimate timeline restart (segment sharding, sequential outputs) is not a
// re-run and must keep its baseline, while a real re-run must not leave the
// dead attempt's downtime in the ETA. The percent floor is not touched: it
// spans the whole job.
func (p *ProgressParser) StartAttempt() {
	p.attemptStartTime = p.now()
	p.attemptFrames = 0
	p.etaSamples = append(p.etaSamples[:0], etaSample{wall: p.attemptStartTime})
	p.ewmaSpeed = 0
}

// Reset resets the parser state for a new job.
func (p *ProgressParser) Reset() {
	p.startTime = p.now()
	p.lastPushTime = time.Time{}
	p.setDurationUs(0)
	p.attemptStartTime = time.Time{}
	p.attemptFrames = 0
	p.etaSamples = nil
	p.ewmaSpeed = 0
	p.maxPercent = -1
	p.hasSeekWindow = false
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

// SeekWindow captures the -ss/-t/-to trims around an ffmpeg command line.
// Input-side and output-side values are kept apart because they apply to
// different timelines: -ss before -i moves the input start, while ffmpeg
// rewrites the output timestamps to start at zero either way, so the two
// sections compose into the span time= counts through. Each field is in
// microseconds; a zero value means the option was absent.
type SeekWindow struct {
	InSeekUs int64 // input-side -ss: seek point inside the input
	InTus    int64 // input-side -t: media read from the seek point
	InToUs   int64 // input-side -to: absolute stop position inside the input
	SeekUs   int64 // output-side -ss: seek point inside the input window
	Tus      int64 // output-side -t: media written
	ToUs     int64 // output-side -to: absolute stop position inside the input window
}

// WindowUs returns the media span the output covers, given the full input
// duration: the input section's span narrowed by the output section's trim.
// time= counts through this span from zero in every -ss/-t/-to combination,
// so it is what percent and ETA must be measured against.
func (sw SeekWindow) WindowUs(inputDurationUs int64) int64 {
	windowUs := trimWindowUs(inputDurationUs, sw.InSeekUs, sw.InTus, sw.InToUs)
	if windowUs == 0 {
		// An empty input span leaves nothing to write, and the output
		// section's -t must not resurrect it.
		return 0
	}
	return trimWindowUs(windowUs, sw.SeekUs, sw.Tus, sw.ToUs)
}

// trimWindowUs narrows durationUs by one section's -ss/-t/-to. -to is an
// absolute timestamp clamped to the section, -ss moves the start forward, and
// -t caps the remainder. -t and -to are mutually exclusive and -t wins: a
// present -t makes -to inert, as ffmpeg does. A -ss past the section's end
// leaves an empty span that -t cannot reopen.
func trimWindowUs(durationUs, seekUs, tUs, toUs int64) int64 {
	windowUs := durationUs
	if tUs == 0 && toUs > 0 {
		windowUs = toUs
		if windowUs > durationUs {
			windowUs = durationUs
		}
	}
	if seekUs > 0 {
		if seekUs >= windowUs {
			windowUs = 0
		} else {
			windowUs -= seekUs
		}
	}
	if tUs > 0 && tUs < windowUs {
		windowUs = tUs
	}
	return windowUs
}

// ParseSeekWindow extracts the input-side and output-side -ss/-t/-to values
// from an ffmpeg command line. An option may appear as "-ss value",
// "-ss=value", or "-ss:value" (stream-specifier form). Input options bind to
// the input they precede, so only the section between the previous input's
// URL and the last -i applies to this output. Only the exact token "-i"
// declares an input — "-i"-prefixed options like -init_hw_device,
// -ignore_unknown and -id3v2_version are output-section options.
func ParseSeekWindow(args []string) SeekWindow {
	var sw SeekWindow
	lastI, prevI := -1, -1
	for i := 0; i < len(args); i++ {
		if args[i] != "-i" {
			continue
		}
		prevI = lastI
		lastI = i
		i++ // the input URL follows -i and is never an option
	}
	if lastI < 0 {
		// No input declared: everything on the line is an output option.
		scanSeekOptions(args, &sw, false)
		return sw
	}
	inputStart := 0
	if prevI >= 0 {
		inputStart = prevI + 2
	}
	scanSeekOptions(args[inputStart:lastI], &sw, true)
	outputStart := lastI + 2
	if outputStart < len(args) {
		scanSeekOptions(args[outputStart:], &sw, false)
	}
	return sw
}

// scanSeekOptions records the -ss/-t/-to options of one section of the
// command line, as input-side or output-side values.
func scanSeekOptions(args []string, sw *SeekWindow, inputSide bool) {
	for i := 0; i < len(args); i++ {
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
			switch {
			case base == "ss" && inputSide:
				sw.InSeekUs = us
			case base == "t" && inputSide:
				sw.InTus = us
			case base == "to" && inputSide:
				sw.InToUs = us
			case base == "ss":
				sw.SeekUs = us
			case base == "t":
				sw.Tus = us
			case base == "to":
				sw.ToUs = us
			}
		}
	}
}

// FilterProgressLine returns true if a line contains progress information.
func FilterProgressLine(line string) bool {
	return strings.Contains(line, "time=")
}

// ffmpegStatsLineRe matches ffmpeg's -stats progress updates written to stderr:
// video ("frame= ... fps= ... time= ..."), audio-only ("size= ..."), and the
// final lines ("frame= ... Lsize= ..." / "Lsize= ..."). The size/Lsize value is
// "N/A" for non-file outputs (-f null -, pipes, rtmp, …), so it accepts either
// a digit or N/A. These lines are rewritten in place with \r by ffmpeg and are
// already surfaced to the CLI as structured progress pushes by ProgressRouter,
// so forwarding the raw line interleaves frame/fps noise with the [rffmpeg]
// log channel.
var ffmpegStatsLineRe = regexp.MustCompile(`^\s*(?:frame=\s*\d|(?:size|Lsize)=\s*(?:\d|N/A)).*time=\d`)

// ffmpegSummaryLineRe matches ffmpeg's terminal -stats summary line emitted
// once per job at exit: "video:...kB audio:...kB ... muxing overhead: ...%"
// (audio-only jobs omit the video: column; ffmpeg 7 prefixes the line with
// "[out#N/fmt @ 0x...]"). It carries no progress information and is raw
// ffmpeg output, so it must not reach the CLI log stream either.
var ffmpegSummaryLineRe = regexp.MustCompile(`^\s*(?:\[out#\d+[^\]]*\]\s+)?(?:video|audio):.*muxing overhead:`)

// isFFmpegStatsLine reports whether line is ffmpeg -stats output — an
// incremental progress update or the terminal summary — that must not be
// forwarded verbatim to the CLI log stream.
func isFFmpegStatsLine(line string) bool {
	return ffmpegStatsLineRe.MatchString(line) || ffmpegSummaryLineRe.MatchString(line)
}
