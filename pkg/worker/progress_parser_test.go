package worker

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
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

	// Each case gets its own parser: ParseLine drops frames reporting less than
	// the job's high-water mark, so cases must not share one parser's mark.
	for _, tt := range tests {
		p := NewProgressParser()
		p.SetDuration(100_000_000) // 100 seconds in microseconds

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

func TestIsFFmpegStatsLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "video stats",
			line: "frame=  100 fps= 30 q=28.0 size=    1024kB time=00:00:03.33 bitrate=2518.0kbits/s speed=1x",
			want: true,
		},
		{
			name: "video final Lsize",
			line: "frame=  100 fps=30 q=-1.0 Lsize=  500kB time=00:00:03.33 bitrate=2045.0kbits/s speed=2.05x",
			want: true,
		},
		{
			name: "audio stats",
			line: "size=    1024kB time=00:00:03.33 bitrate=2518.0kbits/s speed=1x",
			want: true,
		},
		{
			name: "audio final Lsize",
			line: "Lsize=    500kB time=00:00:03.33 bitrate=2045.0kbits/s speed=2.05x",
			want: true,
		},
		{
			name: "zero frame",
			line: "frame=    0 fps=0.0 q=0.0 size=       0kB time=00:00:00.00 bitrate=N/A speed=0.00x",
			want: true,
		},
		{
			name: "audio stats size N/A",
			line: "size=     N/A time=00:00:03.33 bitrate=N/A speed=1x",
			want: true,
		},
		{
			name: "audio final Lsize N/A",
			line: "Lsize=     N/A time=00:00:03.33 bitrate=N/A speed=1x",
			want: true,
		},
		{
			name: "null output size N/A",
			line: "size=N/A time=00:00:00.20 bitrate=N/A speed= 284x",
			want: true,
		},
		{
			name: "muxing overhead summary",
			line: "video:2KiB audio:0KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: 35.5%",
			want: true,
		},
		{
			name: "muxing overhead summary ffmpeg7 prefix",
			line: "[out#0/mpegts @ 0x55d1a2b3c4d0] video:2KiB audio:0KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: 35.5%",
			want: true,
		},
		{
			name: "audio only muxing overhead",
			line: "audio:1000kB subtitle:0kB other streams:0kB global headers:0kB muxing overhead: 0.00%",
			want: true,
		},
		{
			name: "rffmpeg notification",
			line: "[rffmpeg] Cache hit: deadbeef",
			want: false,
		},
		{
			name: "rffmpeg rewrite",
			line: "[rffmpeg] upgraded libx264 → h264_nvenc",
			want: false,
		},
		{
			name: "retry notice",
			line: "[RETRY] Stage 1 succeeded with encoder h264_nvenc",
			want: false,
		},
		{
			name: "ffmpeg stream header",
			line: "  Stream #0:0: Video: h264 (libx264)",
			want: false,
		},
		{
			name: "ffmpeg error",
			line: "Error while opening encoder for output stream #0:0",
			want: false,
		},
		{
			name: "plain log",
			line: "Just a regular log message",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFFmpegStatsLine(tt.line); got != tt.want {
				t.Errorf("isFFmpegStatsLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
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

// TestProgressParser_ETAPrecision_BoundaryChecks tests ETA edge cases:
// start of encoding (time=0), near-completion, and speed=0.
func TestProgressParser_ETAPrecision_BoundaryChecks(t *testing.T) {
	// Edge case 1: Start of encoding (time=0.00s).
	// speed=1.0x but time=0, so ETA should not be calculated (timeUs > 0 check).
	p := NewProgressParser()
	p.SetDuration(int64(30) * 1_000_000)
	frame := p.ParseLine("frame=0 fps=0 q=0.0 size=0kB time=00:00:00.00 bitrate=N/A speed=1.00x")
	if frame == nil {
		t.Fatal("expected non-nil frame for time=0 line")
	}
	if frame.EtaSeconds != 0 {
		t.Errorf("time=0 should produce ETA=0 (or unknown), got %d", frame.EtaSeconds)
	}

	// Edge case 2: speed=0 (no progress, should not crash).
	frame2 := p.ParseLine("frame=100 time=00:00:05.00 bitrate=N/A speed=0.00x")
	if frame2 == nil {
		t.Fatal("expected non-nil frame")
	}
	// ETA should be 0 since speed=0 is not > 0.
	if frame2.EtaSeconds != 0 {
		t.Errorf("speed=0 should not produce ETA, got %d", frame2.EtaSeconds)
	}
}

// encoderTimeline replays the progress stream one ffmpeg attempt produces, so
// an estimate can be scored against a finish the parser never sees. Every
// field is the simulation's own: no bound or constant is read from the
// implementation, which is what makes a wrong estimate visible here.
type encoderTimeline struct {
	durationSeconds   float64 // media length
	rate              float64 // steady-state media seconds per wall second
	outputLagSeconds  float64 // media encoded but not yet published in time=
	startDelaySeconds float64 // wall clock before the first stats line
	finalizeSeconds   float64 // wall clock from the last frame to process exit
}

// publishedAt is the media time= reports at wall seconds since the attempt
// started: the pipeline holds outputLagSeconds of output back, and the
// closing burst releases it.
func (tl encoderTimeline) publishedAt(wall float64) float64 {
	published := (wall-tl.startDelaySeconds)*tl.rate - tl.outputLagSeconds
	if published < 0 {
		published = 0
	}
	if published > tl.durationSeconds {
		published = tl.durationSeconds
	}
	return published
}

// encodeEndWall is when the last frame is encoded.
func (tl encoderTimeline) encodeEndWall() float64 {
	return tl.startDelaySeconds + tl.durationSeconds/tl.rate
}

// endWall is when the process exits, which is what the CLI's clock measures.
func (tl encoderTimeline) endWall() float64 {
	return tl.encodeEndWall() + tl.finalizeSeconds
}

// timelineSample is one -stats line during a replayed attempt.
type timelineSample struct {
	published float64 // media time= reported
	remaining float64 // true wall clock to process exit
	eta       int     // ETA the parser rendered, 0 when it certified nothing
}

// replayTimeline feeds a parser one -stats line per step of wall clock, the
// way the Worker does, and returns every frame it produced.
func replayTimeline(t *testing.T, tl encoderTimeline, step float64) []timelineSample {
	t.Helper()
	p := NewProgressParser()
	p.SetDuration(int64(tl.durationSeconds * 1_000_000))
	wall := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wall*1e9))
	}
	p.StartAttempt()

	var samples []timelineSample
	for wall = tl.startDelaySeconds; wall <= tl.endWall()+1e-9; wall += step {
		published := tl.publishedAt(wall)
		if wall >= tl.encodeEndWall() {
			// The closing stats burst publishes everything the pipeline held.
			published = tl.durationSeconds
		}
		frame := p.ParseLine(buildProgressLine(int64(published*1_000_000), tl.rate))
		if frame == nil {
			continue
		}
		samples = append(samples, timelineSample{
			published: published,
			remaining: tl.endWall() - wall,
			eta:       frame.EtaSeconds,
		})
	}
	return samples
}

// TestProgressParser_ETACertifiedWithinTolerance is the contract check: every
// ETA the parser renders must land within 20% of the wall clock the process
// actually took, across the output-lag and encode-rate range the pipeline
// produces. The output lag is the simulation's own parameter — a fixed
// deduction tuned to one of these rows fails the others, which is the point.
func TestProgressParser_ETACertifiedWithinTolerance(t *testing.T) {
	base := encoderTimeline{
		durationSeconds:   120,
		startDelaySeconds: 1,
		finalizeSeconds:   0.6,
	}
	for _, lag := range []float64{0, 1.4, 2.6} {
		for _, rate := range []float64{1, 2, 5.68} {
			tl := base
			tl.outputLagSeconds, tl.rate = lag, rate
			name := fmt.Sprintf("lag%.1fs/rate%.2fx", lag, rate)

			rendered := 0
			for _, s := range replayTimeline(t, tl, 1.0) {
				if s.eta == 0 || s.remaining < 3 {
					continue
				}
				rendered++
				errPct := math.Abs(float64(s.eta)-s.remaining) / s.remaining * 100
				if errPct >= 20 {
					t.Errorf("%s: ETA %ds with %.2fs actually left is %.1f%% off (bound is 20%%)",
						name, s.eta, s.remaining, errPct)
				}
			}
			if rendered == 0 {
				t.Errorf("%s: no ETA rendered at all; the estimator must still report a job it can certify", name)
			}
		}
	}
}

// TestProgressParser_ETAAvailabilityOnAcceptanceJob pins the other half of the
// contract: suppression must not swallow the tail so completely that the
// acceptance script stops asserting anything. At the throughput the QA
// harness measured, the estimate has to stay available well inside the last
// ten seconds — the script needs three numeric samples there.
func TestProgressParser_ETAAvailabilityOnAcceptanceJob(t *testing.T) {
	tl := encoderTimeline{
		durationSeconds:   120,
		rate:              5.68,
		outputLagSeconds:  2.0,
		startDelaySeconds: 0.5,
		finalizeSeconds:   0.6,
	}
	rendered := 0
	for _, s := range replayTimeline(t, tl, 1.0) {
		if s.eta == 0 || s.remaining < 3 {
			continue
		}
		rendered++
		if errPct := math.Abs(float64(s.eta)-s.remaining) / s.remaining * 100; errPct >= 20 {
			t.Errorf("ETA %ds with %.2fs left is %.1f%% off", s.eta, s.remaining, errPct)
		}
	}
	if rendered < 3 {
		t.Errorf("only %d numeric ETA samples in the last seconds; the acceptance script requires 3", rendered)
	}
}

// TestProgressParser_ETACoversObservedRateDrift replays a pace step at each
// amplitude the acceptance run showed and beyond, in both directions. A step
// needs about one rate window to cross the sample window, so while the oldest
// chunks still hold the old rate the estimate must carry that spread as
// uncertainty; certifying the extremes passed +30% steps at 21.9%. Samples
// before the change are not judged: no sample-based estimate can know a pace
// that has not happened yet.
func TestProgressParser_ETACoversObservedRateDrift(t *testing.T) {
	const (
		durationSec = 600
		baseRate    = 4.56
		switchSec   = 30.0
	)
	for _, factor := range []float64{1.3, 1.5, 0.7, 0.6} {
		t.Run(fmt.Sprintf("x%.2f", factor), func(t *testing.T) {
			rate := baseRate * factor
			mediaAt := func(wall float64) float64 {
				if wall <= switchSec {
					return wall * baseRate
				}
				return switchSec*baseRate + (wall-switchSec)*rate
			}
			totalWallSec := switchSec + (durationSec-switchSec*baseRate)/rate

			p := NewProgressParser()
			p.SetDuration(durationSec * 1_000_000)
			wallSec := 0.0
			p.nowFunc = func() time.Time {
				return time.Unix(0, int64(wallSec*float64(time.Second)))
			}
			p.StartAttempt()

			emitted := 0
			for wall := 1.0; wall <= totalWallSec; wall++ {
				wallSec = wall
				frame := p.ParseLine(buildProgressLine(int64(mediaAt(wall)*1_000_000), rate))
				if frame == nil {
					t.Fatalf("frame at %.0fs: nil", wall)
				}
				if frame.EtaSeconds == 0 || wall < switchSec+1 {
					continue
				}
				remaining := totalWallSec - wall
				if remaining < 3 {
					continue
				}
				emitted++
				if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
					t.Errorf("ETA %ds at %.0fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
						frame.EtaSeconds, wall, remaining, errPct)
				}
			}
			if emitted < 10 {
				t.Fatalf("only %d certified samples after the pace change; the rate uncertainty must not swallow the run", emitted)
			}
		})
	}
}

// TestProgressParser_ETAAvailabilityUnderJitter replays a pace that alternates
// between two plateaus a ratio of 1.3 apart, the swing the acceptance host
// shows. Both plateaus are real states of the process, so the estimate is the
// window mean rather than whichever plateau the newest chunks happen to sit
// on: a window holding both levels certifies a value between them, not one of
// them. The phase sweep matters — with 5s plateaus and 2s chunks the newest
// pair lands on one level for 0-2s at a time, and a pair-only estimate missed
// the other level by 25% at one phase while the same fixture passed at others.
// The clip is long enough to hold two full cycles: a one-off departure and a
// swing look identical until the departed level returns, so the mean is only
// trusted after both levels have been observed on both sides of the window.
func TestProgressParser_ETAAvailabilityUnderJitter(t *testing.T) {
	const (
		durationSec = 600.0
		fastRate    = 5.68
		ratio       = 1.3
		plateauSec  = 5.0
	)
	for _, offset := range []float64{0, 1, 2, 3, 4} {
		t.Run(fmt.Sprintf("phase%.0fs", offset), func(t *testing.T) {
			rateAt := func(wall float64) float64 {
				if int((wall+offset)/plateauSec)%2 == 0 {
					return fastRate
				}
				return fastRate / ratio
			}
			mediaAt := func(wall float64) float64 {
				media := 0.0
				for x := 0.0; x < wall; x += 0.01 {
					media += rateAt(x) * 0.01
				}
				return media
			}
			timeToFinish := func(wall float64) float64 {
				remain := durationSec - mediaAt(wall)
				x := wall
				for remain > 0 {
					remain -= rateAt(x) * 0.01
					x += 0.01
				}
				return x - wall
			}

			p := NewProgressParser()
			p.SetDuration(int64(durationSec) * 1_000_000)
			wallSec := 0.0
			p.nowFunc = func() time.Time {
				return time.Unix(0, int64(wallSec*float64(time.Second)))
			}
			p.StartAttempt()

			rendered := 0
			for wall := 0.0; wall < 60; wall++ {
				wallSec = wall
				frame := p.ParseLine(buildProgressLine(int64(mediaAt(wall)*1_000_000), rateAt(wall)))
				if frame == nil {
					t.Fatalf("frame at %.0fs: nil", wall)
				}
				if frame.EtaSeconds == 0 {
					continue
				}
				remaining := timeToFinish(wall)
				if remaining < 3 {
					continue
				}
				rendered++
				if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
					t.Errorf("ETA %ds at %.0fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
						frame.EtaSeconds, wall, remaining, errPct)
				}
			}
			if rendered < 3 {
				t.Fatalf("only %d numeric ETA samples under a 1.3 pace swing; the acceptance script requires 3", rendered)
			}
		})
	}
}

// TestProgressParser_ContentionBurstNotCertified replays the shape a
// preempted CPU produces: the pace drops for several seconds and then
// recovers. While the rate window still mixes the two levels, the window mean
// is not the pace the finish runs at — certifying it reported 21-30% high in
// this fixture, the same direction as the acceptance run's 25.4% sample. Those
// samples must render n/a, and once the old level has left the window the
// estimate must come back on the recovered pace.
func TestProgressParser_ContentionBurstNotCertified(t *testing.T) {
	const (
		durationSec = 300.0
		fastRate    = 4.6
		slowRate    = 3.3
		burstStart  = 6.0
		burstLen    = 8.0
	)
	rateAt := func(wall float64) float64 {
		if wall >= burstStart && wall < burstStart+burstLen {
			return slowRate
		}
		return fastRate
	}
	mediaAt := func(wall float64) float64 {
		media := 0.0
		for x := 0.0; x < wall; x += 0.01 {
			media += rateAt(x) * 0.01
		}
		return media
	}
	timeToFinish := func(wall float64) float64 {
		remain := durationSec - mediaAt(wall)
		x := wall
		for remain > 0 {
			remain -= rateAt(x) * 0.01
			x += 0.01
		}
		return x - wall
	}

	p := NewProgressParser()
	p.SetDuration(int64(durationSec) * 1_000_000)
	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*float64(time.Second)))
	}
	p.StartAttempt()

	rendered := 0
	for wall := 0.0; wall <= 60; wall += 0.5 {
		wallSec = wall
		frame := p.ParseLine(buildProgressLine(int64(mediaAt(wall)*1_000_000), rateAt(wall)))
		if frame == nil {
			t.Fatalf("frame at %.1fs: nil", wall)
		}
		// From the moment the burst ends until its level has slid out of the
		// 10s window, the samples describe a step, not a pace.
		if wall >= burstStart+burstLen-2 && wall <= burstStart+burstLen+9 {
			if frame.EtaSeconds != 0 {
				t.Errorf("ETA %ds at %.1fs while the preemption level still shared the rate window",
					frame.EtaSeconds, wall)
			}
			continue
		}
		if frame.EtaSeconds == 0 {
			continue
		}
		remaining := timeToFinish(wall)
		if remaining < 3 {
			continue
		}
		rendered++
		if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
			t.Errorf("ETA %ds at %.1fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
				frame.EtaSeconds, wall, remaining, errPct)
		}
	}
	if rendered == 0 {
		t.Fatal("no ETA after the preemption level left the window; the guard must recover")
	}
}

// TestProgressParser_FrozenMediaNotCertified covers a window whose chunks
// rateBetween could not measure: media freezes for eight seconds and then
// resumes at the same pace. The measurable chunks at the window's edges each
// contain one second of stall and agree on half the active rate, so a band
// built only from them is zero-width and certifies a finish twice as far away
// as the truth. The window's whole-throughput floor counts the stalled time,
// and no value is emitted until the freeze has left the window.
func TestProgressParser_FrozenMediaNotCertified(t *testing.T) {
	const (
		durationSec = 600.0
		rate        = 4.0
		freezeStart = 30.0
		freezeEnd   = 38.0
	)
	mediaAt := func(wall float64) float64 {
		switch {
		case wall <= freezeStart:
			return wall * rate
		case wall <= freezeEnd:
			return freezeStart * rate
		default:
			return freezeStart*rate + (wall-freezeEnd)*rate
		}
	}
	totalWallSec := freezeEnd + (durationSec-freezeStart*rate)/rate

	p := NewProgressParser()
	p.SetDuration(int64(durationSec) * 1_000_000)
	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*float64(time.Second)))
	}
	p.StartAttempt()

	rendered, recovered := 0, 0
	for wall := 0.0; wall <= 70; wall += 0.5 {
		wallSec = wall
		frame := p.ParseLine(buildProgressLine(int64(mediaAt(wall)*1_000_000), rate))
		if frame == nil {
			t.Fatalf("frame at %.1fs: nil", wall)
		}
		if frame.EtaSeconds == 0 {
			continue
		}
		remaining := totalWallSec - wall
		if remaining < 3 {
			continue
		}
		rendered++
		if wall >= freezeEnd+etaRateWindowSeconds {
			recovered++
		}
		if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
			t.Errorf("ETA %ds at %.1fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
				frame.EtaSeconds, wall, remaining, errPct)
		}
	}
	if rendered == 0 {
		t.Fatal("no ETA at all; the guard must report a job it can certify")
	}
	if recovered == 0 {
		t.Fatal("no ETA after the freeze left the window; the guard must recover")
	}
}

// TestProgressParser_SparseSamplesNotCertified pins the other half of the
// guard: a progress stream too sparse to fill two rate chunks carries no
// dispersion, so it cannot bound the finish. The old fallback certified the
// whole-window slope with a zero-width band; the contract requires n/a until
// the stream supports a range.
func TestProgressParser_SparseSamplesNotCertified(t *testing.T) {
	p := NewProgressParser()
	p.SetDuration(300 * 1_000_000)
	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*float64(time.Second)))
	}
	p.StartAttempt()

	for wall := 0.0; wall <= 60; wall += 5 {
		wallSec = wall
		frame := p.ParseLine(buildProgressLine(int64(wall*5.0*1_000_000), 5.0))
		if frame == nil {
			t.Fatalf("frame at %.0fs: nil", wall)
		}
		if frame.EtaSeconds != 0 {
			t.Fatalf("ETA %ds at %.0fs from a single rate measurement", frame.EtaSeconds, wall)
		}
	}
}

// TestProgressParser_ETATransientLoadDrop replays the other shape the
// acceptance host showed: a 4.5s drop to 4.0x that recovers, both edges of it
// inside one rate window. After the drop lands the newest chunks all agree on
// the slow level, and certifying it while the window still holds the fast one
// missed by 40%. The window mean is the pace a finish crossing the dip runs
// at, so every value rendered must stay inside the bound.
func TestProgressParser_ETATransientLoadDrop(t *testing.T) {
	const (
		durationSec = 120.0
		fastRate    = 5.68
		slowRate    = 4.0
		dipStart    = 12.0
		dipLen      = 4.5
	)
	rateAt := func(wall float64) float64 {
		if wall >= dipStart && wall < dipStart+dipLen {
			return slowRate
		}
		return fastRate
	}
	mediaAt := func(wall float64) float64 {
		media := 0.0
		for x := 0.0; x < wall; x += 0.01 {
			media += rateAt(x) * 0.01
		}
		return media
	}
	timeToFinish := func(wall float64) float64 {
		remain := durationSec - mediaAt(wall)
		x := wall
		for remain > 0 {
			remain -= rateAt(x) * 0.01
			x += 0.01
		}
		return x - wall
	}

	p := NewProgressParser()
	p.SetDuration(int64(durationSec) * 1_000_000)
	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*float64(time.Second)))
	}
	p.StartAttempt()

	rendered := 0
	for wall := 0.0; wall < 60; wall++ {
		wallSec = wall
		frame := p.ParseLine(buildProgressLine(int64(mediaAt(wall)*1_000_000), rateAt(wall)))
		if frame == nil {
			t.Fatalf("frame at %.0fs: nil", wall)
		}
		if frame.EtaSeconds == 0 {
			continue
		}
		remaining := timeToFinish(wall)
		if remaining < 3 {
			continue
		}
		rendered++
		if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
			t.Errorf("ETA %ds at %.0fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
				frame.EtaSeconds, wall, remaining, errPct)
		}
	}
	if rendered == 0 {
		t.Fatal("no ETA across the whole run; the dip must not silence the estimate forever")
	}
}

// TestProgressParser_ETACoversObservedDrift replays a monotonic ramp — the
// other shape the acceptance host showed (23s of run against 37.7s left, a
// 39% miss). Adjacent chunks differ by about 1% here, which the settled check
// cannot see; the drift is projected over the remaining run instead, and
// whatever is certified must stay inside the bound.
func TestProgressParser_ETACoversObservedDrift(t *testing.T) {
	const baseRate = 4.56
	// A falling ramp cannot outlast the point where its rate reaches zero, so
	// the two directions get a media length their drift can still finish.
	for _, tc := range []struct {
		perTen   float64
		duration float64
		atLeast  int
	}{
		{0.05, 600, 1}, {0.08, 600, 1}, {-0.05, 280, 1},
		// The steepest decay certifies nothing: the projection widens the
		// finish interval as the rate keeps falling, so no integer second
		// fits inside 20% of both ends on this clip. The row still guards
		// the bound for any sample it does render.
		{-0.08, 280, 0},
	} {
		perTen, durationSec, wantAtLeast := tc.perTen, tc.duration, tc.atLeast
		t.Run(fmt.Sprintf("%+.0f%%/10s", perTen*100), func(t *testing.T) {
			slope := perTen / 10.0 // relative rate change per wall second
			mediaAt := func(wall float64) float64 {
				return baseRate*wall + 0.5*baseRate*slope*wall*wall
			}
			totalWallSec := 0.0
			for mediaAt(totalWallSec) < durationSec {
				totalWallSec += 0.01
			}

			p := NewProgressParser()
			p.SetDuration(int64(durationSec) * 1_000_000)
			wallSec := 0.0
			p.nowFunc = func() time.Time {
				return time.Unix(0, int64(wallSec*float64(time.Second)))
			}
			p.StartAttempt()

			emitted := 0
			for wall := 0.0; wall <= totalWallSec; wall++ {
				wallSec = wall
				frame := p.ParseLine(buildProgressLine(int64(mediaAt(wall)*1_000_000), baseRate*(1+slope*wall)))
				if frame == nil {
					t.Fatalf("frame at %.0fs: nil", wall)
				}
				if frame.EtaSeconds == 0 {
					continue
				}
				remaining := totalWallSec - wall
				if remaining < 3 {
					continue
				}
				emitted++
				if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
					t.Errorf("ETA %ds at %.0fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
						frame.EtaSeconds, wall, remaining, errPct)
				}
			}
			if emitted < wantAtLeast {
				t.Fatalf("only %d ETA samples on the ramp; want at least %d", emitted, wantAtLeast)
			}
		})
	}
}

// TestProgressParser_ETASuppressedAtUncertifiableTail replays the sample that
// broke the acceptance run: 18.24s of media still unpublished at a settled
// 4.56x, with a little over 3s of wall clock actually left. 20% of that is
// less than a pipeline tail can hide, so no integer second can be certified
// and the frame must carry no ETA (C-23 renders n/a) instead of a guess. The
// grid is fine enough to land on the sample itself: at 1s steps the nearest
// point is 21.68s of media left, where the bound is wide enough to pass and
// the test would prove nothing.
func TestProgressParser_ETASuppressedAtUncertifiableTail(t *testing.T) {
	tl := encoderTimeline{
		durationSeconds:   120,
		rate:              4.56,
		outputLagSeconds:  2.0,
		startDelaySeconds: 1.0,
		finalizeSeconds:   0.6,
	}
	var atSample *timelineSample
	samples := replayTimeline(t, tl, 0.05)
	for i := range samples {
		if samples[i].published <= 101.76 {
			atSample = &samples[i]
		}
	}
	if atSample == nil {
		t.Fatal("timeline never reached the acceptance sample")
	}
	if math.Abs(atSample.published-101.76) > 0.05 {
		t.Fatalf("replay landed on %.2fs of media, want the 18.24s-left sample", 120-atSample.published)
	}
	if atSample.eta != 0 {
		t.Errorf("at %.2fs of media left the samples cannot certify a finish, got ETA %ds",
			120-atSample.published, atSample.eta)
	}
	// The same attempt still reports an ETA earlier on, where the bound fits.
	reported := 0
	for _, s := range samples {
		if s.eta != 0 {
			reported++
		}
	}
	if reported == 0 {
		t.Errorf("no ETA anywhere in the attempt")
	}
}

// TestProgressParser_StartupRampNotCertified verifies that the start-up ramp
// never certifies an estimate: nothing is emitted until the attempt has
// published enough media to be past the ramp and has a full rate window
// behind it, and everything emitted afterwards stays inside the contract's
// 20% bound. The 12s case is the falsifier — it is longer than the 8s
// wall-clock gate the estimator used to have, and that version certified a
// four-times-too-high value 11s in.
func TestProgressParser_StartupRampNotCertified(t *testing.T) {
	const (
		durationSec  = 120
		rampSpeed    = 0.5
		settledSpeed = 2.0
	)
	for _, rampSec := range []float64{6, 10, 12, 15} {
		t.Run(fmt.Sprintf("ramp%.0fs", rampSec), func(t *testing.T) {
			totalWallSec := rampSec + (durationSec-rampSec*rampSpeed)/settledSpeed

			p := NewProgressParser()
			p.SetDuration(durationSec * 1_000_000)
			wallSec := 0.0
			p.nowFunc = func() time.Time {
				return time.Unix(0, int64(wallSec*float64(time.Second)))
			}
			p.StartAttempt()

			mediaAt := func(wall float64) float64 {
				if wall <= rampSec {
					return wall * rampSpeed
				}
				return rampSec*rampSpeed + (wall-rampSec)*settledSpeed
			}

			emitted := 0
			for wall := 0.0; wall <= totalWallSec; wall++ {
				wallSec = wall
				speed := settledSpeed
				if wall <= rampSec {
					speed = rampSpeed
				}
				frame := p.ParseLine(buildProgressLine(int64(mediaAt(wall)*1_000_000), speed))
				if frame == nil {
					t.Fatalf("frame at %.0fs: nil", wall)
				}
				if frame.EtaSeconds == 0 {
					continue
				}
				emitted++
				if wall <= rampSec {
					t.Errorf("ETA %ds at %.0fs, inside the %.0fs start-up ramp", frame.EtaSeconds, wall, rampSec)
					continue
				}
				remaining := totalWallSec - wall
				if remaining < 3 {
					continue
				}
				if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
					t.Errorf("ETA %ds at %.0fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
						frame.EtaSeconds, wall, remaining, errPct)
				}
			}
			if emitted == 0 {
				t.Fatal("no ETA after the ramp; the estimator must still report a job it can certify")
			}
		})
	}
}

// TestProgressParser_LowFrameRateFillNotCertified verifies the settle gate is
// counted in frames as well as in media. At 3fps the 20s media gate is
// reached while the encoder is still filling its pipeline, and the rate there
// would extrapolate a finish several times too early; the frame gate holds
// until the fill is over, and everything emitted after it stays inside the
// contract's bound.
func TestProgressParser_LowFrameRateFillNotCertified(t *testing.T) {
	const (
		fps          = 3
		durationSec  = 120.0
		rampSec      = 45.0
		rampSpeed    = 0.5
		settledSpeed = 2.0
	)
	totalWallSec := rampSec + (durationSec-rampSec*rampSpeed)/settledSpeed

	p := NewProgressParser()
	p.SetDuration(int64(durationSec) * 1_000_000)
	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*float64(time.Second)))
	}
	p.StartAttempt()

	emitted := 0
	for wall := 0.0; wall <= totalWallSec; wall++ {
		wallSec = wall
		media := wall * rampSpeed
		speed := rampSpeed
		if wall > rampSec {
			media = rampSec*rampSpeed + (wall-rampSec)*settledSpeed
			speed = settledSpeed
		}
		frame := p.ParseLine(buildProgressLineFPS(int64(media*1_000_000), speed, fps))
		if frame == nil {
			t.Fatalf("frame at %.0fs: nil", wall)
		}
		if frame.EtaSeconds == 0 {
			continue
		}
		if wall <= rampSec {
			t.Errorf("ETA %ds at %.0fs, inside the %.0fs start-up fill", frame.EtaSeconds, wall, rampSec)
			continue
		}
		remaining := totalWallSec - wall
		if remaining < 3 {
			continue
		}
		emitted++
		if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
			t.Errorf("ETA %ds at %.0fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
				frame.EtaSeconds, wall, remaining, errPct)
		}
	}
	if emitted == 0 {
		t.Fatal("no ETA after the start-up fill; the estimator must still report a job it can certify")
	}
}

// TestProgressParser_ETAAvailabilityOnShortJobs bounds what the estimator
// offers a job that ends inside its own guard band: the rate window needs a
// full window of run and the tail needs a few seconds the bound can certify,
// so a short clip may legitimately render n/a throughout (C-23's legal value,
// not a missed bound). What it may never do is render a value that misses.
// The 40s/2x and 60s/2x runs are long enough to owe one; the shorter clips
// stay free to improve while wrong values are still caught.
func TestProgressParser_ETAAvailabilityOnShortJobs(t *testing.T) {
	cases := []struct {
		duration    float64
		rate        float64
		wantAtLeast int
	}{
		{duration: 15, rate: 5.68},
		{duration: 25, rate: 5.68},
		{duration: 40, rate: 2.0, wantAtLeast: 1},
		{duration: 60, rate: 5.68},
		{duration: 60, rate: 2.0, wantAtLeast: 1},
	}
	for _, tc := range cases {
		tl := encoderTimeline{
			durationSeconds:   tc.duration,
			rate:              tc.rate,
			outputLagSeconds:  2.0,
			startDelaySeconds: 0.5,
			finalizeSeconds:   0.6,
		}
		rendered := 0
		for _, s := range replayTimeline(t, tl, 1.0) {
			if s.eta == 0 || s.remaining < 3 {
				continue
			}
			rendered++
			if errPct := math.Abs(float64(s.eta)-s.remaining) / s.remaining * 100; errPct >= 20 {
				t.Errorf("%.0fs@%.2fx: ETA %ds with %.1fs left is %.1f%% off", tc.duration, tc.rate, s.eta, s.remaining, errPct)
			}
		}
		t.Logf("%.0fs@%.2fx: %d numeric samples", tc.duration, tc.rate, rendered)
		if rendered < tc.wantAtLeast {
			t.Errorf("%.0fs@%.2fx: %d numeric samples, want at least %d", tc.duration, tc.rate, rendered, tc.wantAtLeast)
		}
	}
}

// TestProgressParser_ETATracksRecentRate verifies the estimate follows the
// encoder's current pace. A job that starts far faster than its settled rate
// must not report the start's pace: while the fast and slow chunks share the
// window the pace has not settled and nothing is emitted; once it has, every
// value must match the 4x pace within the contract's bound.
func TestProgressParser_ETATracksRecentRate(t *testing.T) {
	const durationSec = 120
	p := NewProgressParser()
	p.SetDuration(durationSec * 1_000_000)

	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*float64(time.Second)))
	}
	p.StartAttempt()

	// 8x for the first six wall seconds, then a steady 4x.
	mediaAt := func(wall float64) float64 {
		if wall <= 6 {
			return wall * 8
		}
		return 48 + (wall-6)*4
	}
	totalWallSec := 6 + (durationSec-48)/4.0
	feed := func(wall float64) *ProgressFrame {
		wallSec = wall
		speed := 4.0
		if wall <= 6 {
			speed = 8.0
		}
		return p.ParseLine(buildProgressLine(int64(mediaAt(wall)*1_000_000), speed))
	}

	emitted := 0
	for wall := 1.0; wall <= totalWallSec; wall++ {
		frame := feed(wall)
		if frame == nil {
			t.Fatalf("frame at %.0fs: nil", wall)
		}
		if frame.EtaSeconds == 0 {
			continue
		}
		// The 8x start is still one chunk of the window through wall 9; a
		// value then would be extrapolating the old pace.
		if wall < 10 {
			t.Fatalf("ETA %ds at %.0fs emitted while the 8x start still sets the pace", frame.EtaSeconds, wall)
		}
		remaining := totalWallSec - wall
		if remaining < 3 {
			continue
		}
		emitted++
		if errPct := math.Abs(float64(frame.EtaSeconds)-remaining) / remaining * 100; errPct >= 20 {
			t.Errorf("ETA %ds at %.0fs with %.1fs actually left is %.1f%% off (bound is 20%%)",
				frame.EtaSeconds, wall, remaining, errPct)
		}
	}
	if emitted == 0 {
		t.Fatal("no ETA once the 4x pace settled")
	}
}

// TestProgressParser_ResetClearsSampleHistory verifies Reset drops the ETA
// attempt state: the next job must build its rate from its own samples, not
// from the previous job's pace.
func TestProgressParser_ResetClearsSampleHistory(t *testing.T) {
	p := NewProgressParser()
	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*1e9))
	}

	// Job A runs at 1x and settles its estimate around 1x.
	p.SetDuration(100 * 1_000_000)
	var last *ProgressFrame
	for i := int64(1); i <= 30; i++ {
		wallSec = float64(i - 1)
		line := fmt.Sprintf("frame=%d fps=30 q=28.0 size=1024kB time=00:00:%02d.00 bitrate=2045.0kbits/s speed=1.00x", i*30, i)
		last = p.ParseLine(line)
		if last == nil {
			t.Fatalf("job A frame %d: dropped", i)
		}
	}
	if last.EtaSeconds < 56 || last.EtaSeconds > 84 {
		t.Fatalf("job A: ETA %ds with 70s left at 1x", last.EtaSeconds)
	}

	// Job B is four times faster. Its first estimate must follow that pace:
	// a leftover sample history would hold the rate range around 1x and push
	// the value four times too high.
	p.Reset()
	p.SetDuration(100 * 1_000_000)
	for i := int64(1); i <= 45; i++ {
		wallSec = 30 + float64(i-1)/4
		line := fmt.Sprintf("frame=%d fps=30 q=28.0 size=1024kB time=00:00:%02d.00 bitrate=2045.0kbits/s speed=4.00x", i*30, i)
		f := p.ParseLine(line)
		if f == nil || f.EtaSeconds == 0 {
			continue
		}
		remaining := 100.0 - float64(i)
		if float64(f.EtaSeconds) < remaining/4*0.8 || float64(f.EtaSeconds) > remaining/4*1.2 {
			t.Fatalf("job B frame %d: ETA %ds, want about %.0fs at 4x", i, f.EtaSeconds, remaining/4)
		}
		return
	}
	t.Fatal("job B never produced an ETA")
}

// TestProgressParser_WallClockETA_WarmupOverhead verifies the wall-clock-rate
// ETA stays within 20% for a non-trimmed task with significant encoder
// warm-up overhead — the scenario that failed with the old EWMA-speed formula
// (+37% early, -42% late). It simulates a 120s task (5s warm-up at 0.5x, then
// steady 2.0x): the wall-clock model absorbs the warm-up, so the ETA error is
// largest at early checkpoints (22% at 28% progress) and drops well below 20%
// as more data accumulates.
func TestProgressParser_WallClockETA_WarmupOverhead(t *testing.T) {
	// 120s media, 5s warm-up at 0.5x, then steady at 2.0x.
	durationUs := int64(120) * 1_000_000
	p := NewProgressParser()
	p.SetDuration(durationUs)

	// Wall clock simulation.
	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*1e9))
	}

	// Warm-up: 2.5s media in 5s wall (0.5x speed).
	type sample struct {
		mediaS float64
		wallS  float64
		speed  string
	}
	samples := []sample{
		{0.5, 1.0, "0.50"},     // warm-up
		{1.5, 3.0, "0.50"},     // warm-up
		{2.5, 5.0, "0.50"},     // warm-up end
		{7.0, 7.25, "2.00"},    // 28% progress area
		{34.0, 20.75, "2.00"},  // ~28% — QA failure point
		{60.0, 33.75, "2.00"},  // 50%
		{96.0, 51.75, "2.00"},  // 80%
		{108.0, 57.75, "2.00"}, // 90%
	}

	for i, s := range samples {
		wallSec = s.wallS
		totalSec := int64(s.mediaS)
		ms := int64(s.mediaS*100) % 100
		line := fmt.Sprintf("frame=%d fps=30 q=28.0 size=1024kB time=00:00:%02d.%02d bitrate=2045.0kbits/s speed=%sx",
			totalSec*30, totalSec, ms, s.speed)
		frame := p.ParseLine(line)
		if frame == nil {
			t.Fatalf("sample %d: nil frame", i)
		}
		// Once the attempt is past its start-up ramp and has a full rate
		// window behind it, the ETA appears and is progressively more
		// accurate. We only assert for samples past 28% (the QA failure
		// point).
		pct := frame.Percent
		if pct >= 28 && frame.EtaSeconds > 0 {
			// Total wall time = 5 + (120-2.5)/2.0 = 63.75s
			totalWallSec := 5.0 + (120.0-2.5)/2.0
			actualRemaining := totalWallSec - s.wallS
			if actualRemaining <= 0 {
				continue
			}
			errPct := absIntF(frame.EtaSeconds, actualRemaining) / actualRemaining * 100
			t.Logf("sample %d: %.0f%% media=%.1fs wall=%.1fs ETA=%ds actual=%.1fs err=%.1f%%",
				i, pct, s.mediaS, s.wallS, frame.EtaSeconds, actualRemaining, errPct)
			if errPct > 20 {
				t.Errorf("sample %d (%.0f%%): ETA error %.1f%% exceeds 20%%", i, pct, errPct)
			}
		}
	}
}

// absIntF returns the absolute value of a-b as a float64.
func absIntF(a int, b float64) float64 {
	d := float64(a) - b
	if d < 0 {
		return -d
	}
	return d
}

// TestParseFFmpegDurationUs verifies parsing of ffmpeg duration specs in
// both decimal-seconds and HH:MM:SS[.ms] timecode forms.
func TestParseFFmpegDurationUs(t *testing.T) {
	cases := []struct {
		input string
		want  int64
	}{
		{"30", 30_000_000},
		{"30.5", 30_500_000},
		{"1.25", 1_250_000},
		{"00:00:30", 30_000_000},
		{"00:01:30", 90_000_000},
		{"01:00:00", 3600_000_000},
		{"00:00:00.5", 500_000},
		{"00:00:10.250", 10_250_000},
		{"", 0},
		{"abc", 0},
		{"-5", 0},
	}
	for _, tc := range cases {
		got := parseFFmpegDurationUs(tc.input)
		if got != tc.want {
			t.Errorf("parseFFmpegDurationUs(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

// TestParseSeekWindow verifies that output-side -ss/-t/-to are extracted
// from ffmpeg args while input-side -ss (before -i) is ignored.
func TestParseSeekWindow(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want SeekWindow
	}{
		{
			name: "no trim options",
			args: []string{"-y", "-i", "input.mp4", "-c:v", "libx264", "out.mp4"},
			want: SeekWindow{},
		},
		{
			name: "output -t only",
			args: []string{"-y", "-i", "input.mp4", "-t", "30", "out.mp4"},
			want: SeekWindow{Tus: 30_000_000},
		},
		{
			name: "output -ss and -t",
			args: []string{"-y", "-i", "input.mp4", "-ss", "00:00:10", "-t", "30", "out.mp4"},
			want: SeekWindow{SeekUs: 10_000_000, Tus: 30_000_000},
		},
		{
			name: "output -ss and -to",
			args: []string{"-y", "-i", "input.mp4", "-ss", "10", "-to", "40", "out.mp4"},
			want: SeekWindow{SeekUs: 10_000_000, ToUs: 40_000_000},
		},
		{
			name: "input-side -ss ignored",
			args: []string{"-y", "-ss", "10", "-i", "input.mp4", "out.mp4"},
			want: SeekWindow{},
		},
		{
			name: "inline = form",
			args: []string{"-y", "-i", "input.mp4", "-ss=10", "-t=30", "out.mp4"},
			want: SeekWindow{SeekUs: 10_000_000, Tus: 30_000_000},
		},
		{
			name: "stream specifier form -ss:0",
			args: []string{"-y", "-i", "input.mp4", "-ss:0", "10", "out.mp4"},
			want: SeekWindow{SeekUs: 10_000_000},
		},
		{
			name: "decimal seconds -t",
			args: []string{"-y", "-i", "input.mp4", "-t", "0.5", "out.mp4"},
			want: SeekWindow{Tus: 500_000},
		},
		{
			name: "-ss with -ignore_unknown after (regression: -i prefix heuristic)",
			args: []string{"-y", "-i", "input.mp4", "-ss", "10", "-ignore_unknown", "-t", "30", "out.mp4"},
			want: SeekWindow{SeekUs: 10_000_000, Tus: 30_000_000},
		},
		{
			name: "-ss with -init_hw_device after (regression: -i prefix heuristic)",
			args: []string{"-y", "-i", "input.mp4", "-ss", "10", "-init_hw_device", "foo", "-t", "30", "out.mp4"},
			want: SeekWindow{SeekUs: 10_000_000, Tus: 30_000_000},
		},
		{
			name: "-id3v2_version not mistaken for -i",
			args: []string{"-y", "-i", "input.mp4", "-ss", "10", "-id3v2_version", "4", "-t", "30", "out.mp4"},
			want: SeekWindow{SeekUs: 10_000_000, Tus: 30_000_000},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSeekWindow(tc.args)
			if got != tc.want {
				t.Errorf("ParseSeekWindow() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestProgressParser_SeekWindow_Percent verifies that percent reaches 100%
// for a trimmed job when time= matches the output window.
func TestProgressParser_SeekWindow_Percent(t *testing.T) {
	// 120s input, -ss 10 -t 30 → output window = 30s.
	inputDur := int64(120) * 1_000_000
	p := NewProgressParser()
	p.SetDuration(inputDur)
	p.SetSeekWindow(inputDur, 10_000_000, 30_000_000, 0)

	// At time=00:00:15 (15s into the 30s segment) → 50%.
	frame := p.ParseLine("frame=100 fps=30 q=28.0 time=00:00:15.00 speed=1.0x")
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.Percent < 49 || frame.Percent > 51 {
		t.Errorf("at 15s/30s window: percent=%.1f, want ~50", frame.Percent)
	}

	// At time=00:00:30 (end of window) → 100%.
	frame = p.ParseLine("frame=200 fps=30 q=28.0 time=00:00:30.00 speed=1.0x")
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.Percent != 100 {
		t.Errorf("at 30s/30s window: percent=%.1f, want 100", frame.Percent)
	}
}

// TestProgressParser_SeekWindow_NoTrim_Reaches100 verifies that without
// trim options the parser still reaches 100% (regression: SetSeekWindow
// with all-zero args must not break the full-duration path).
func TestProgressParser_SeekWindow_NoTrim_Reaches100(t *testing.T) {
	inputDur := int64(120) * 1_000_000
	p := NewProgressParser()
	p.SetDuration(inputDur)
	p.SetSeekWindow(inputDur, 0, 0, 0)

	frame := p.ParseLine("frame=200 fps=30 q=28.0 time=00:02:00.00 speed=1.0x")
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.Percent != 100 {
		t.Errorf("at 120s/120s: percent=%.1f, want 100", frame.Percent)
	}
}

// TestProgressParser_SeekWindow_ToAndSs verifies -to with -ss produces a
// window of (to - ss).
func TestProgressParser_SeekWindow_ToAndSs(t *testing.T) {
	// 120s input, -ss 10 -to 40 → window = 30s.
	inputDur := int64(120) * 1_000_000
	p := NewProgressParser()
	p.SetDuration(inputDur)
	p.SetSeekWindow(inputDur, 10_000_000, 0, 40_000_000)

	// At time=00:00:15 → 50% of the 30s window.
	frame := p.ParseLine("frame=100 fps=30 q=28.0 time=00:00:15.00 speed=1.0x")
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.Percent < 49 || frame.Percent > 51 {
		t.Errorf("at 15s/30s window: percent=%.1f, want ~50", frame.Percent)
	}
}

// TestProgressParser_SeekWindow_TClampsToInput verifies -t larger than the
// remaining input is clamped (does not produce a window > input).
func TestProgressParser_SeekWindow_TClampsToInput(t *testing.T) {
	// 120s input, -ss 100 -t 500 → window clamped to 20s (120-100).
	inputDur := int64(120) * 1_000_000
	p := NewProgressParser()
	p.SetDuration(inputDur)
	p.SetSeekWindow(inputDur, 100_000_000, 500_000_000, 0)

	frame := p.ParseLine("frame=100 fps=30 q=28.0 time=00:00:10.00 speed=1.0x")
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.Percent < 49 || frame.Percent > 51 {
		t.Errorf("at 10s/20s window: percent=%.1f, want ~50", frame.Percent)
	}
}

// TestProgressParser_SeekWindow_Degenerate verifies that -ss beyond -to
// sets durationUs to 0 so percent reports -1 (unknown), not a ratio against
// the full input duration.
func TestProgressParser_SeekWindow_Degenerate(t *testing.T) {
	// 120s input, -ss 50 -to 40 → seek beyond end, nothing to encode.
	inputDur := int64(120) * 1_000_000
	p := NewProgressParser()
	p.SetDuration(inputDur)
	p.SetSeekWindow(inputDur, 50_000_000, 0, 40_000_000)

	if got := p.DurationUs(); got != 0 {
		t.Errorf("degenerate window: DurationUs() = %d, want 0", got)
	}
	frame := p.ParseLine("frame=0 fps=0 q=0.0 time=00:00:00.00 speed=0.00x")
	if frame == nil {
		t.Fatal("expected non-nil frame")
	}
	if frame.Percent != -1 {
		t.Errorf("degenerate window: percent = %.1f, want -1 (unknown)", frame.Percent)
	}
}

// TestParseFFmpegDurationUs_TrailingColon verifies that a trailing-colon
// timecode (e.g. "00:01:") is rejected rather than silently parsed as 60s.
func TestParseFFmpegDurationUs_TrailingColon(t *testing.T) {
	if got := parseFFmpegDurationUs("00:01:"); got != 0 {
		t.Errorf("parseFFmpegDurationUs(\"00:01:\") = %d, want 0 (invalid)", got)
	}
	if got := parseFFmpegDurationUs("00:"); got != 0 {
		t.Errorf("parseFFmpegDurationUs(\"00:\") = %d, want 0 (invalid)", got)
	}
}

// TestProgressRouter_SendFinal verifies that SendFinal sends a 100% progress
// update to the server.
func TestProgressRouter_SendFinal(t *testing.T) {
	var mu sync.Mutex
	var gotPercent float64
	var gotEta int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req protocol.JobUpdateRequest
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		gotPercent = req.Progress
		gotEta = req.EtaSeconds
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "worker-1", "")
	router := NewProgressRouter(client, "job-1", func(line string) {})
	router.SetDuration(60_000_000)

	router.SendFinal()

	mu.Lock()
	defer mu.Unlock()
	if gotPercent != 100 {
		t.Errorf("SendFinal percent = %.1f, want 100", gotPercent)
	}
	if gotEta != 0 {
		t.Errorf("SendFinal eta = %d, want 0", gotEta)
	}
}

// TestProgressRouterHandlerFiltersStatsLines verifies the Worker→CLI stream
// drops ffmpeg -stats frame/fps lines while forwarding everything else — the
// structured progress pushes replace them, so the CLI log channel stays free
// of raw frame=... fps=... noise interleaved with [rffmpeg] lines.
func TestProgressRouterHandlerFiltersStatsLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var forwarded []string
	client := NewClient(srv.URL, "worker-1", "")
	router := NewProgressRouter(client, "job-1", func(line string) {
		mu.Lock()
		forwarded = append(forwarded, line)
		mu.Unlock()
	})

	h := router.Handler()
	h("Input #0, mov,mp4:\n")
	h("[rffmpeg] Cache hit: deadbeef\n")
	h("frame=  100 fps= 30 q=28.0 size=    1024kB time=00:00:03.33 bitrate=2518.0kbits/s speed=1x\n")
	h("frame=  200 fps= 30 q=28.0 size=    2048kB time=00:00:06.66 bitrate=2518.0kbits/s speed=1x\n")
	h("size=    1024kB time=00:00:06.66 bitrate=2518.0kbits/s speed=1x\n")
	h("size=N/A time=00:00:06.66 bitrate=N/A speed= 284x\n")
	h("video:2KiB audio:0KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: 35.5%\n")
	h("[rffmpeg] fallback to software encoder libx264\n")

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"Input #0, mov,mp4:\n",
		"[rffmpeg] Cache hit: deadbeef\n",
		"[rffmpeg] fallback to software encoder libx264\n",
	}
	if len(forwarded) != len(want) {
		t.Fatalf("forwarded %d lines, want %d: %q", len(forwarded), len(want), forwarded)
	}
	for i := range want {
		if forwarded[i] != want[i] {
			t.Errorf("forwarded[%d] = %q, want %q", i, forwarded[i], want[i])
		}
	}
}

// TestProgressRouter_ProgressNeverRegresses pins the job-level progress
// contract: the percents pushed to the server never decrease. A retryable
// ffmpeg death mid-encode (OOM/SIGABRT, process_crash) makes the worker's
// multi-stage retry re-run ffmpeg from scratch on the same parser, so the
// media timeline restarts at ~0. The restarted attempt must not drag the
// progress already displayed back down — that rewind is the reported
// 71.1% → 0.5% sequence.
func TestProgressRouter_ProgressNeverRegresses(t *testing.T) {
	var mu sync.Mutex
	var percents []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req protocol.JobUpdateRequest
		if err := json.Unmarshal(body, &req); err == nil {
			mu.Lock()
			percents = append(percents, req.Progress)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "worker-1", "")
	router := NewProgressRouter(client, "job-1", func(string) {})
	router.SetDuration(60_000_000) // 60s input, what probeInputDurationUs seeds
	router.sendDelay = 0           // the 1s throttle is not what this test pins

	h := router.Handler()
	// Attempt 1 encodes to 9.3% of the 60s input before ffmpeg dies.
	h(buildProgressLine(2_000_000, 2.0) + "\n") // 3.3%
	h(buildProgressLine(5_600_000, 2.0) + "\n") // 9.3%
	// The retry re-runs ffmpeg from scratch: time= restarts at zero.
	h(buildProgressLine(1_900_000, 2.0) + "\n")  // 3.2% — must not be pushed
	h(buildProgressLine(5_800_000, 2.0) + "\n")  // 9.7%
	h(buildProgressLine(12_000_000, 2.0) + "\n") // 20.0%

	mu.Lock()
	defer mu.Unlock()
	if len(percents) < 4 {
		t.Fatalf("pushed %d progress updates, want >= 4: %v", len(percents), percents)
	}
	for i := 1; i < len(percents); i++ {
		if percents[i] < percents[i-1] {
			t.Fatalf("progress regressed at update %d: %v — a restarted attempt must not rewind the job's progress", i, percents)
		}
	}
	last := percents[len(percents)-1]
	if last < 19.99 || last > 20.01 {
		t.Errorf("final pushed percent = %v, want 20 (the last frame's own value)", last)
	}
}

// TestProgressParser_ResetClearsProgressFloor: the monotonic floor is
// per-job state, so Reset must clear it — a parser reused for a new job must
// not stay pinned to the previous job's high-water mark.
func TestProgressParser_ResetClearsProgressFloor(t *testing.T) {
	p := NewProgressParser()
	p.SetDuration(60_000_000)

	mid := p.ParseLine(buildProgressLine(30_000_000, 2.0))
	if mid == nil || mid.Percent < 49.99 || mid.Percent > 50.01 {
		t.Fatalf("first frame percent = %v, want ~50", mid)
	}
	if late := p.ParseLine(buildProgressLine(1_000_000, 2.0)); late != nil {
		t.Fatalf("regressing frame accepted mid-job: percent=%.1f", late.Percent)
	}

	p.Reset()
	p.SetDuration(60_000_000)
	restarted := p.ParseLine(buildProgressLine(1_000_000, 2.0))
	if restarted == nil || restarted.Percent < 1.66 || restarted.Percent > 1.67 {
		t.Fatalf("after Reset the new job's first frame was dropped: %v", restarted)
	}
}

// TestProgressParser_DurationChangeRebasesFloor: the floor is measured in
// percent, so a denominator change re-bases it. Without that, a late header
// Duration: line that grows the denominator would leave every later frame below
// the old floor and the progress would stall for the rest of the job even
// though the media time keeps advancing.
func TestProgressParser_DurationChangeRebasesFloor(t *testing.T) {
	p := NewProgressParser()
	p.SetDuration(60_000_000)

	if f := p.ParseLine(buildProgressLine(18_000_000, 2.0)); f == nil || f.Percent < 29.99 || f.Percent > 30.01 {
		t.Fatalf("first frame percent = %v, want ~30", f)
	}

	// The denominator grows to three times the seeded value (the input
	// header's Duration: overwrites what the probe seeded), so the same media
	// position now reads as a lower percent.
	p.SetDuration(180_000_000)
	f := p.ParseLine(buildProgressLine(21_000_000, 2.0))
	if f == nil {
		t.Fatal("frame after the denominator change was dropped: progress would stall below the old floor")
	}
	if f.Percent < 11.66 || f.Percent > 11.67 {
		t.Fatalf("percent after the re-base = %v, want ~11.7", f.Percent)
	}
}

// TestProgressParser_AttemptBoundaryRebasesETABaseline: StartAttempt — what the
// retry executor reports before each ffmpeg re-run — re-bases the wall-clock
// baseline, so a restarted attempt's ETA is not derived from a clock that still
// includes the dead attempt's downtime. A timeline that merely goes backward
// without that boundary (a legitimate restart such as segment sharding or
// sequential outputs) keeps its baseline. The percent floor is unaffected
// either way: a restarted attempt stays off the wire until it catches up.
func TestProgressParser_AttemptBoundaryRebasesETABaseline(t *testing.T) {
	run := func(t *testing.T, newAttemptBoundary bool) int {
		t.Helper()
		p := NewProgressParser()
		p.SetDuration(600_000_000)
		wallSec := 0.0
		p.nowFunc = func() time.Time {
			return time.Unix(0, int64(wallSec*1e9))
		}

		// Attempt 1 advances with a live wall-clock rate. -stats lines
		// arrive about once a second, so the settled window exists by 18s.
		p.ParseLine(buildProgressLine(30_000_000, 2.0)) // seeds the baseline at wall 0
		for sec := 1; sec <= 17; sec++ {
			wallSec = float64(sec)
			p.ParseLine(buildProgressLine(30_000_000+int64(sec)*13_000_000/18, 2.0))
		}
		wallSec = 18
		if f := p.ParseLine(buildProgressLine(43_000_000, 2.0)); f == nil || f.EtaSeconds <= 0 {
			t.Fatalf("attempt 1 ETA = %v, want > 0", f)
		}

		// ffmpeg dies; the next attempt re-runs from zero a second later.
		wallSec = 19
		if newAttemptBoundary {
			p.StartAttempt()
		}
		if f := p.ParseLine(buildProgressLine(300_000, 2.0)); f != nil {
			t.Fatalf("restarted attempt's frame was pushed: percent=%.1f", f.Percent)
		}

		// Past the mark, the retry's ETA is only free of the dead attempt's
		// clock when the boundary was reported.
		wallSec = 20
		f := p.ParseLine(buildProgressLine(43_500_000, 2.0))
		if f == nil {
			t.Fatal("retry frame above the mark was dropped")
		}
		return f.EtaSeconds
	}

	if eta := run(t, true); eta != 0 {
		t.Errorf("ETA right after a reported attempt boundary = %ds, want 0 (the dead attempt's clock must not be used)", eta)
	}
	if eta := run(t, false); eta <= 0 {
		t.Errorf("ETA after a bare time= rewind = %ds, want > 0 (a legitimate timeline restart keeps its baseline)", eta)
	}

	// Once the restarted attempt has run past the settle gate and built its
	// own rate window, the ETA returns, computed from that attempt's pace.
	// The retry re-runs from zero, so its stats stream is fed densely.
	p := NewProgressParser()
	p.SetDuration(600_000_000)
	wallSec := 0.0
	p.nowFunc = func() time.Time {
		return time.Unix(0, int64(wallSec*1e9))
	}
	p.ParseLine(buildProgressLine(30_000_000, 2.0))
	wallSec = 18
	p.StartAttempt()
	mediaUs := int64(300_000)
	var f *ProgressFrame
	for sec := 18.0; sec <= 42; sec++ {
		wallSec = sec
		mediaUs += 2_000_000
		f = p.ParseLine(buildProgressLine(mediaUs, 2.0))
	}
	if f == nil {
		t.Fatal("retry frame above the mark was dropped")
	}
	if f.EtaSeconds <= 0 {
		t.Errorf("ETA after the restarted attempt's rate window = %ds, want > 0", f.EtaSeconds)
	}
}

// TestProgressRouter_HeaderDurationDoesNotWidenSeekWindow: for a trimmed job the
// output window is the denominator, so the input header's Duration: line — which
// the router feeds through SetDuration on every job — must not re-base the
// percent onto the full input. Doing so would read a finished trimmed segment as
// a few percent, and with the monotonic floor in place the job would then stall
// there for the rest of the run.
func TestProgressRouter_HeaderDurationDoesNotWidenSeekWindow(t *testing.T) {
	var mu sync.Mutex
	var percents []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req protocol.JobUpdateRequest
		if err := json.Unmarshal(body, &req); err == nil {
			mu.Lock()
			percents = append(percents, req.Progress)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "worker-1", "")
	router := NewProgressRouter(client, "job-1", func(string) {})
	router.sendDelay = 0 // the throttle is not what this test pins

	// -ss 240 -t 60 over a 5-minute input: a 60s output window.
	router.SetDuration(300_000_000)
	router.SetSeekWindow(300_000_000, 240_000_000, 60_000_000, 0)

	h := router.Handler()
	h("  Duration: 00:05:00.00, start: 0.000000, bitrate: 1000 kb/s\n")
	h(buildProgressLine(30_000_000, 2.0) + "\n") // 30s into the 60s window

	mu.Lock()
	defer mu.Unlock()
	if len(percents) != 1 {
		t.Fatalf("pushed %d progress updates, want 1: %v", len(percents), percents)
	}
	if percents[0] < 49.99 || percents[0] > 50.01 {
		t.Errorf("percent = %v, want ~50 (the 60s output window, not the 5-minute input)", percents[0])
	}
}

// buildProgressLine creates a synthetic ffmpeg progress line for testing.
// buildProgressLineFPS formats the same stats line for a source at the given
// frame rate: frame= is the 3fps gate's input, so it cannot be hard-coded.
func buildProgressLineFPS(timeUs int64, speed float64, fps int) string {
	totalSec := int(timeUs / 1_000_000)
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	ms := (timeUs % 1_000_000) / 10_000
	totalKB := timeUs / 1000
	return fmt.Sprintf("frame=%d fps=%d q=28.0 size=%dkB time=%02d:%02d:%02d.%02d bitrate=2045.0kbits/s speed=%.2fx",
		int(float64(timeUs)/1e6*float64(fps)), fps, totalKB, h, m, s, ms, speed)
}

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
