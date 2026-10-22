package worker

import (
	"fmt"
	"math"
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
	p.hasSeekWindow = true
	p.setDurationUs(windowUs)
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

// recordETASample appends one progress observation and drops history that is
// too old to serve as the rate window's base. At least two samples are kept:
// a sparse stream still needs the previous observation to measure a span.
func (p *ProgressParser) recordETASample(now time.Time, mediaUs int64) {
	p.etaSamples = append(p.etaSamples, etaSample{wall: now, mediaUs: mediaUs})
	cutoff := now.Add(-time.Duration(2 * etaRateWindowSeconds * float64(time.Second)))
	for len(p.etaSamples) > 2 && p.etaSamples[0].wall.Before(cutoff) {
		p.etaSamples = p.etaSamples[1:]
	}
}

// etaRates is the throughput evidence one sample window supports.
type etaRates struct {
	pace       float64 // rate the finish is projected from
	spread     float64 // dispersion observed around that rate
	newest     float64 // rate over the newest chunk
	oldest     float64 // rate over the oldest chunk: where the window started
	isTrending bool    // chunk rates drift in one direction without a level step
	settled    bool    // one pace describes the window, or a swing's mean does
}

// etaRateRange returns the throughput evidence the recent samples support.
//
// A single slope hides the error a mixed window creates: it lands between the
// old and the new rate. The window is split into 2s chunks instead. One level
// is the pace right now; a level that changed for good withholds the estimate
// until the old one slides out; a level that keeps coming back is a swing and
// its mean predicts the finish. Reading only the newest pair let a two-level
// window report zero dispersion and certify one level (a 1.3 swing, +25%).
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
	if len(chunkRates) == 0 {
		// A sparse stream may not fill any chunk; the window slope still is a
		// rate measurement.
		if _, rewound := p.rewindWall(base, len(p.etaSamples)-1); rewound {
			return etaRates{}, false
		}
		whole := float64(mediaUs-p.etaSamples[base].mediaUs) / 1_000_000.0 / span.Seconds()
		if whole <= 0 {
			return etaRates{}, false
		}
		chunkRates = append(chunkRates, whole)
	}

	rates := etaRates{
		pace:   chunkRates[0],
		newest: chunkRates[0],
		oldest: chunkRates[len(chunkRates)-1],
	}
	mean := float64(mediaUs-p.etaSamples[base].mediaUs) / 1_000_000.0 / span.Seconds()
	if mean <= 0 {
		mean = chunkRates[0]
	}

	changed, levelFlips, low, high := rateLevelChange(chunkRates)
	if changed {
		// The window holds more than one level. Reversals mean both levels
		// are visited in turn: jitter around one pace, where the mean
		// predicts and its standard error is the band. A level observed,
		// left and observed again is a swing: the mean predicts a finish
		// longer than the swing, and the band is half the amplitude it
		// showed. A one-way departure whose newest level has landed near the
		// mean is noise-sized too. A departure still crossing the newest
		// chunk, or landing far from the mean, is a step: no finish is
		// certifiable until the old level slides out of the window.
		landed := len(chunkRates) < 2 ||
			math.Abs(chunkRates[0]-chunkRates[1]) <= chunkRates[1]*etaTolerance/2
		nearMean := landed && math.Abs(rates.newest-mean) <= mean*etaTolerance/2
		switch {
		case levelFlips > 0 || nearMean:
			rates.pace = mean
			rates.spread = rateStdDev(chunkRates, mean) / math.Sqrt(float64(len(chunkRates)))
			rates.settled = true
		case p.rateLevelRecurs(now):
			rates.pace = mean
			rates.spread = (high - low) / 2
			rates.settled = true
		}
		return rates, true
	}

	rates.settled = true
	if len(chunkRates) >= 2 {
		previous, newest := chunkRates[1], chunkRates[0]
		rates.spread = math.Abs(newest - previous)
		rates.pace = (newest + previous) / 2
		// The newest chunk is compared with the mean of the preceding three,
		// not with one neighbour: a single chunk straddling a level boundary
		// would otherwise read as a change still arriving and swallow half
		// the samples of a 5s swing.
		recent, count := 0.0, 0
		for i := 1; i < len(chunkRates) && count < 3; i++ {
			recent += chunkRates[i]
			count++
		}
		recent /= float64(count)
		if math.Abs(newest-recent) > recent*etaTolerance/2 {
			rates.settled = false
		}
	}
	// A drift is monotonic and moves in small steps; a level change moves in
	// one large step and is not projected forward.
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
	return rates, true
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
// disagree by more than half the error budget, how many of those changes
// reverse direction, and the window's low and high rate.
func rateLevelChange(rates []float64) (bool, int, float64, float64) {
	low, high := rates[0], rates[0]
	changed, flips, lastSign := false, 0, 0
	for i, r := range rates {
		if r < low {
			low = r
		}
		if r > high {
			high = r
		}
		if i == 0 || math.Abs(r-rates[i-1]) <= rates[i-1]*etaTolerance/2 {
			continue
		}
		changed = true
		sign := 1
		if r < rates[i-1] {
			sign = -1
		}
		if lastSign != 0 && sign != lastSign {
			flips++
		}
		lastSign = sign
	}
	return changed, flips, low, high
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

// rateLevelRecurs reports whether the newest chunk rate appears again after
// the history departed from it for more than one chunk. A level that returns
// is a swing; one that never returns is a step.
func (p *ProgressParser) rateLevelRecurs(now time.Time) bool {
	span := 2 * time.Duration(etaRateWindowSeconds*float64(time.Second))
	base := len(p.etaSamples) - 1
	for base > 0 && now.Sub(p.etaSamples[base-1].wall) <= span {
		base--
	}
	history := p.chunkRatesFrom(base, now)
	if len(history) < 3 {
		return false
	}
	level := history[0]
	near := func(r float64) bool {
		return math.Abs(r-level) <= level*etaTolerance/2
	}
	i := 0
	for i < len(history) && near(history[i]) {
		i++
	}
	// A single chunk away from the level is noise, not a departure.
	if i+1 >= len(history) || near(history[i+1]) {
		return false
	}
	for i < len(history) && !near(history[i]) {
		i++
	}
	return i < len(history)
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
func (p *ProgressParser) etaEstimate(now time.Time, mediaUs, durationUs int64) int {
	rates, ok := p.etaRateRange(now, mediaUs)
	if !ok {
		return 0
	}
	// The evidence is measured backwards, so while a pace change crosses the
	// window the newest chunks already show the new rate while the older ones
	// still hold the old one. The two newest chunks are the pace as it is
	// right now; when they disagree by more than half the error budget the
	// change is still arriving, and no finish can be certified until it has
	// landed (C-23 renders n/a meanwhile).
	if !rates.settled {
		return 0
	}
	remaining := float64(durationUs-mediaUs) / 1_000_000.0
	if remaining <= 0 {
		return 0
	}
	// The newest pair is the pace as it is now, and the band around it is the
	// dispersion those two chunks actually showed — not the window's whole
	// history. A fixed half-tolerance band would stack with the tail terms and
	// leave no certifiable second on a steady run (measured: zero numeric
	// samples on the acceptance job); the whole-window range would do the same
	// whenever the pace oscillates.
	rateLo := rates.pace - rates.spread/2
	rateHi := rates.pace + rates.spread/2
	if rateLo <= 0 {
		return 0
	}
	// A pace that keeps moving the same way will not be at its current value
	// for the rest of the run. Project the observed drift over the remaining
	// time: the finish lies between "the drift stops now" and "it continues",
	// and that spread is what the interval has to cover.
	if rates.isTrending && rates.oldest > 0 {
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
	if rendered <= 0 {
		return 0
	}
	if float64(rendered) > (1+etaTolerance)*lo {
		return 0
	}
	if float64(rendered) < (1-etaTolerance)*hi {
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
// options (after the last -i) affect the progress window — input-side -ss is
// a demuxer hint and is ignored. The args slice is the full ffmpeg command
// line; boolean/arity decisions use the generated ffmpegopts table so
// -ss/-t/-to are never mistaken for switches.
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
