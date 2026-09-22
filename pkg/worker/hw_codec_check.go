package worker

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// hardwareCodecProbeTimeout bounds a single hardware encoder capability probe.
// The probe is a 1-frame null encode that normally completes in tens of
// milliseconds; the bound only guards against a pathological ffmpeg build that
// hangs during encoder init.
const hardwareCodecProbeTimeout = 10 * time.Second

// hardwareCodecProbeSize/Rate are the synthetic frame the probe encodes. QSV
// rejects a handful of encoder/ratecontrol combinations at tiny resolutions
// (vp9_qsv reports "Current frame rate is unsupported" for 64x64@30), so the
// probe uses a conservative 320x240@30 that every QSV encoder family accepts.
const (
	hardwareCodecProbeSize = "320x240"
	hardwareCodecProbeRate = "30"
)

// hardwareCodecVerdict classifies a probe outcome.
type hardwareCodecVerdict int

const (
	// verdictInconclusive means the probe did not settle the question: it
	// failed for a reason other than a codec gap (transient device failure,
	// parameter mismatch, timeout). Inconclusive verdicts are never cached —
	// caching one would let a single transient fault permanently disable the
	// pre-check and let the silent hang return.
	verdictInconclusive hardwareCodecVerdict = iota
	// verdictSupported means the runtime encoded a frame with the encoder.
	verdictSupported
	// verdictUnsupported means the runtime definitively cannot encode the
	// encoder's codec.
	verdictUnsupported
)

// hardwareCodecResult is one encoder's settled probe outcome.
type hardwareCodecResult struct {
	verdict hardwareCodecVerdict
	reason  string
}

// inflightProbe deduplicates concurrent probes for the same encoder: the first
// caller probes, the rest wait and read the shared result.
type inflightProbe struct {
	done   chan struct{}
	result hardwareCodecResult
}

// HardwareCodecChecker pre-flights a requested hardware encoder against the
// local runtime before the job's ffmpeg is launched. A worker can advertise an
// encoder ffmpeg is compiled with (av1_qsv) while the installed runtime cannot
// actually encode that codec (e.g. AV1 on a Comet Lake iGPU). Without this
// check the job fails inside ffmpeg and silently degrades to a software
// fallback (libaom-av1) whose encode is so slow the job appears to hang at a
// frozen progress value and never reaches a terminal state. The check turns
// that into a fast, deterministic ENCODER_UNSUPPORTED.
type HardwareCodecChecker struct {
	ffmpegPath string

	mu       sync.Mutex
	cache    map[string]hardwareCodecResult
	inflight map[string]*inflightProbe
}

// NewHardwareCodecChecker creates a hardware codec checker. Only conclusive
// verdicts are cached, per encoder, for the worker's lifetime.
func NewHardwareCodecChecker(ffmpegPath string) *HardwareCodecChecker {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	return &HardwareCodecChecker{
		ffmpegPath: ffmpegPath,
		cache:      make(map[string]hardwareCodecResult),
		inflight:   make(map[string]*inflightProbe),
	}
}

// Check reports whether the local runtime definitively cannot encode the given
// encoder's codec. It returns false for software encoders and for any probe
// outcome that is not an unambiguous codec-capability gap, so a transient
// device error or a parameter-only probe failure never blocks a job.
func (c *HardwareCodecChecker) Check(ctx context.Context, encoder string) (unsupported bool, reason string) {
	if !isHardwareEncoderByName(encoder) {
		return false, ""
	}

	for {
		c.mu.Lock()
		if r, ok := c.cache[encoder]; ok {
			c.mu.Unlock()
			return r.verdict == verdictUnsupported, r.reason
		}
		if p, ok := c.inflight[encoder]; ok {
			// Another job is already probing this encoder. Wait for its
			// verdict instead of spawning a duplicate probe.
			c.mu.Unlock()
			select {
			case <-p.done:
				return p.result.verdict == verdictUnsupported, p.result.reason
			case <-ctx.Done():
				return false, ""
			}
		}

		p := &inflightProbe{done: make(chan struct{})}
		c.inflight[encoder] = p
		c.mu.Unlock()

		verdict, reason := c.probe(ctx, encoder)
		p.result = hardwareCodecResult{verdict: verdict, reason: reason}

		c.mu.Lock()
		delete(c.inflight, encoder)
		if verdict != verdictInconclusive {
			c.cache[encoder] = p.result
		}
		c.mu.Unlock()
		// Publish the result to waiters only after it is fully written.
		close(p.done)

		return verdict == verdictUnsupported, reason
	}
}

// probe runs a 1-frame null encode for the encoder and classifies the outcome.
func (c *HardwareCodecChecker) probe(ctx context.Context, encoder string) (hardwareCodecVerdict, string) {
	probeCtx, cancel := context.WithTimeout(ctx, hardwareCodecProbeTimeout)
	defer cancel()

	args := []string{
		"-y",
		"-hide_banner",
		"-f", "lavfi",
		"-i", "nullsrc=s=" + hardwareCodecProbeSize + ":r=" + hardwareCodecProbeRate,
		"-frames:v", "1",
		"-c:v", encoder,
		"-f", "null",
		"-",
	}
	cmd := exec.CommandContext(probeCtx, c.ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	// Check the codec-gap message before the exit code: ffmpeg has been seen to
	// exit 0 while still reporting the gap on stderr.
	if reason := hardwareCodecUnsupportedReason(stderr.String()); reason != "" {
		return verdictUnsupported, reason
	}
	if err == nil {
		return verdictSupported, ""
	}
	return verdictInconclusive, ""
}

// hardwareCodecUnsupportedReason extracts the stderr line that reports a
// permanent hardware codec gap ("This version of runtime doesn't support AV1
// encoding"). It matches the message text, not the exit code: an encoder can
// exit non-zero for parameter problems the real job may still satisfy
// (vp9_qsv's default ratecontrol), and failing those fast would break
// encoders that work when the job supplies the right flags.
func hardwareCodecUnsupportedReason(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		lower := strings.ToLower(strings.TrimSpace(line))
		if lower == "" {
			continue
		}
		if (strings.Contains(lower, "doesn't support") || strings.Contains(lower, "does not support")) &&
			(strings.Contains(lower, "encoding") || strings.Contains(lower, "codec")) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
