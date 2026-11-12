package worker

import (
	"fmt"
	"math"
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

// durationRegex matches the Duration: HH:MM:SS.ms header line. It is anchored
// to the start of the line with at most the header's two-space indent: the
// same token appears inside a Metadata block (indented under "  Metadata:"),
// and an unanchored match would take that container field for the stream's
// real duration.
var durationRegex = regexp.MustCompile(`^ {0,2}Duration:\s*(\d+):(\d+):(\d+)\.(\d+)`)

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
	// startupStepSeen records that the pace has left its start-up level. The
	// start-up gate then stops applying: the fill is over, and the rows the
	// samples describe after the step may certify without waiting out the
	// fixed bound.
	startupStepSeen bool
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

// etaSample is one progress observation: the wall clock and the percentage
// at the rendered grid (one decimal), so the rate judged is the rate a reader
// of the progress line sees (C-122). The percentage is kept in tenths of a
// point so a block's increment is an exact integer and the interval endpoints
// below compare without float rounding.
type etaSample struct {
	wall      time.Time
	pctTenths int64
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

	// A job's percent never goes backward, so a restarted attempt's frames are
	// dropped instead of rewinding progress the CLI already displayed. The drop
	// happens before the ETA evidence below: a row the CLI never renders is not
	// rate evidence either (C-122, ADR-016). Frames with an unknown duration
	// (-1) carry no percent to compare against and pass through untouched.
	if frame.Percent >= 0 {
		if p.maxPercent >= 0 && frame.Percent < p.maxPercent {
			return nil
		}
		p.maxPercent = frame.Percent
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
		p.recordETASample(now, frame.Percent)
		if float64(timeUs) >= etaSettleMediaSeconds*1_000_000 && p.attemptFrames >= etaSettleFrames {
			frame.EtaSeconds = p.etaEstimate(now, timeUs, durationUs)
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

// recordETASample appends one progress observation and drops history older
// than the rate window plus one block edge: nothing older can enter a block.
// At least two samples are kept: a sparse stream still needs a span.
func (p *ProgressParser) recordETASample(now time.Time, pct float64) {
	rendered := int64(math.Round(pct * 10))
	p.etaSamples = append(p.etaSamples, etaSample{wall: now, pctTenths: rendered})
	keep := etaRateWindowSeconds + etaRateChunkSeconds
	cutoff := now.Add(-time.Duration(keep * float64(time.Second)))
	for len(p.etaSamples) > 2 && p.etaSamples[0].wall.Before(cutoff) {
		p.etaSamples = p.etaSamples[1:]
	}
}

// etaRates is the throughput evidence one sample window supports.
type etaRates struct {
	pace    float64 // rate the finish is projected from
	rateLo  float64 // slowest rate the samples can vouch for
	rateHi  float64 // fastest rate the samples can vouch for
	settled bool    // no step shares the window
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
func (p *ProgressParser) etaRateRange(now time.Time) (etaRates, bool) {
	window := time.Duration(etaRateWindowSeconds * float64(time.Second))
	minSpan := time.Duration(etaRateMinSpanSeconds * float64(time.Second))
	duration := float64(p.durationUs) / 1_000_000.0
	if duration <= 0 {
		return etaRates{}, false
	}
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
	if len(p.etaSamples)-base < 2 {
		return etaRates{}, false
	}
	span := now.Sub(p.etaSamples[base].wall)
	if span < minSpan {
		return etaRates{}, false
	}
	// One rate per row: the row's own block. A window with fewer than two
	// measurable blocks carries no dispersion, so it certifies nothing
	// (C-122 threshold clause).
	blocks := p.blocksFrom(base)
	positive, low, high := 0, 0.0, 0.0
	stepped := false
	var last etaBlock
	for _, b := range blocks {
		if b.dpTenths == 0 {
			continue
		}
		positive++
		if low == 0 || b.rate < low {
			low = b.rate
		}
		if b.rate > high {
			high = b.rate
		}
		// A block without progress produces no rate and is not a rate block,
		// so it does not break the adjacency: the step is judged between the
		// consecutive measurable blocks on either side of it (C-122).
		if last.dpTenths > 0 && paceStep(last, b) {
			stepped = true
		}
		last = b
	}
	if positive < 2 {
		return etaRates{}, false
	}
	// The window's whole throughput counts the time the measurable blocks
	// skipped (a stall). It is the floor a future that repeats the stall
	// would run at; a window that advanced no media bounds nothing.
	windowRate := float64(p.etaSamples[len(p.etaSamples)-1].pctTenths-p.etaSamples[base].pctTenths) / 1000.0 * duration / span.Seconds()
	if windowRate <= 0 {
		return etaRates{}, false
	}
	if stepped && now.Sub(p.attemptStartTime) <= time.Duration(etaStartUpSeconds*float64(time.Second)) {
		// The fill ends at the first pace step; waiting out the fixed bound
		// after the step has been observed would silence rows the samples
		// already describe.
		p.startupStepSeen = true
	}
	sum := 0.0
	for _, b := range blocks {
		if b.dpTenths > 0 {
			sum += b.rate
		}
	}
	rates := etaRates{
		pace:    sum / float64(positive),
		rateLo:  low,
		rateHi:  high,
		settled: !stepped,
	}
	if windowRate < rates.rateLo {
		rates.rateLo = windowRate
	}
	return rates, true
}

// etaBlock is one rate observation: the block ending at one progress row.
// dpTenths is the progress increment in tenths of a percentage point; zero
// means the block measured no rate and is not a rate block (C-122).
type etaBlock struct {
	rate     float64
	dpTenths int64
	span     time.Duration
}

// blocksFrom returns the block of every progress row in [base, last], in row
// order. A row's block ends at that row and starts etaRateChunkSeconds
// earlier; its first row is the sample nearest that start, which may fall
// before the start by one sampling interval (C-122). A block without progress,
// with fewer than two rows, or shorter than 1s of measured wall time produces
// no rate; the zero block is not a rate block, so a step is judged between the
// consecutive measurable blocks on either side of it.
func (p *ProgressParser) blocksFrom(base int) []etaBlock {
	last := len(p.etaSamples) - 1
	blocks := make([]etaBlock, last-base+1)
	duration := float64(p.durationUs) / 1_000_000.0
	if duration <= 0 {
		return nil
	}
	chunk := time.Duration(etaRateChunkSeconds * float64(time.Second))
	minSpan := time.Duration(etaRateChunkSeconds / 2 * float64(time.Second))
	for i := base; i <= last; i++ {
		if i == 0 {
			continue
		}
		start := p.etaSamples[i].wall.Add(-chunk)
		// The window's first block is truncated at the window start: a
		// block never reaches past the rows the window claims (C-122).
		j := base
		for k := base + 1; k < i; k++ {
			if absDuration(p.etaSamples[k].wall.Sub(start)) < absDuration(p.etaSamples[j].wall.Sub(start)) {
				j = k
			}
		}
		span := p.etaSamples[i].wall.Sub(p.etaSamples[j].wall)
		dp := p.etaSamples[i].pctTenths - p.etaSamples[j].pctTenths
		if span < minSpan || dp <= 0 {
			continue
		}
		blocks[i-base] = etaBlock{
			rate:     float64(dp) / 1000.0 * duration / span.Seconds(),
			dpTenths: dp,
			span:     span,
		}
	}
	return blocks
}

// paceStep reports whether two consecutive rate blocks are separated by a pace
// step under C-122: the quantized rate intervals of the two blocks, taken as
// closed intervals, are disjoint, and the point estimates still differ by more
// than etaRateTolerance.
//
// One grid step is 0.1 of a percentage point, so block i's interval is
// r_i × (1 ± 1/dp_i) = (dp_i ± 1) / span_i in common units. With the increment
// in integer tenths and the span in integer nanoseconds, both tests are exact
// integer comparisons: endpoint touching means the intervals intersect, and
// float rounding cannot move either boundary.
func paceStep(prev, cur etaBlock) bool {
	pdp, cdp := prev.dpTenths, cur.dpTenths
	pspan, cspan := int64(prev.span), int64(cur.span)
	disjoint := (pdp+1)*cspan < (cdp-1)*pspan || (cdp+1)*pspan < (pdp-1)*cspan
	if !disjoint {
		return false
	}
	// |cur/prev - 1| > tolerance  <=>  4·cdp·pspan > 5·pdp·cspan or 4·cdp·pspan < 3·pdp·cspan.
	lhs := 4 * cdp * pspan
	rhs := 5 * pdp * cspan
	return lhs > rhs || lhs*5 < rhs*3
}

// absDuration is time.Duration's absolute value.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
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
// [lo, hi]; the estimate is emitted only when an integer second lies within
// etaTolerance of both ends, otherwise the frame carries no ETA and the CLI
// renders n/a (C-23). A number the samples cannot certify is worse than none:
// on the display it is indistinguishable from a correct one.
func (p *ProgressParser) etaEstimate(now time.Time, mediaUs, durationUs int64) int {
	rates, ok := p.etaRateRange(now)
	if !ok {
		return 0
	}
	// The evidence is measured backwards, so while a pace change crosses the
	// window the newest chunks already show the new rate while the older ones
	// still hold the old one. Until the change is a proven oscillation or has
	// slid out of the window, nothing can be certified; meanwhile the CLI
	// renders n/a (C-23).
	if !rates.settled {
		return 0
	}
	// Start-up: the pipeline fills before it runs at the pace the attempt
	// will hold, and the fill ends at a step — the pace leaving its start-up
	// level by more than the rate tolerance. Once that step has been observed
	// the wait is over; a run that never steps is bounded by the pipeline's
	// measured start-up. The bound is scaled for short jobs so they still
	// report (C-122).
	if rates.pace > 0 && !p.startupStepSeen {
		age := now.Sub(p.attemptStartTime).Seconds()
		if remaining := float64(durationUs-mediaUs) / 1_000_000.0; remaining > 0 {
			if projected := age + remaining/rates.pace; age < math.Min(etaStartUpSeconds, etaStartUpFraction*projected) {
				return 0
			}
		}
	}
	remaining := float64(durationUs-mediaUs) / 1_000_000.0
	if remaining <= 0 {
		return 0
	}
	rateLo, rateHi := rates.rateLo, rates.rateHi
	if rateLo <= 0 {
		return 0
	}
	lo := (remaining - etaOutputLagBoundSeconds) / rateHi
	if lo < 0 {
		lo = 0
	}
	hi := remaining/rateLo + etaFinalizeBoundSeconds
	// C-122 item 1: any integer second within the tolerance of both ends
	// certifies the row. Pick the feasible integer closest to the midpoint
	// (the minimax choice), and render n/a only when the feasible set is
	// empty. Requiring the midpoint itself to sit in the band rejects rows
	// that do hold a certifying integer, which is a stricter rule than the
	// contract and turns available ETAs into n/a.
	// AC-40's bound is strict (<20%), so the ends themselves do not
	// certify: the feasible integers lie strictly inside the band.
	feaLo := int(math.Floor((1-etaTolerance)*hi)) + 1
	feaHi := int(math.Ceil((1+etaTolerance)*lo)) - 1
	if feaLo > feaHi {
		return 0
	}
	rendered := int(math.Round((lo + hi) / 2))
	if rendered < feaLo {
		rendered = feaLo
	}
	if rendered > feaHi {
		rendered = feaHi
	}
	if rendered <= 0 {
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
	p.startupStepSeen = false
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
	p.startupStepSeen = false
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
