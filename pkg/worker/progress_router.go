package worker

import (
	"log"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// ProgressRouter intercepts stderr lines, parses progress information,
// and sends periodic progress updates to the server. It transparently
// forwards all lines to the underlying StderrHandler so that regular
// stderr batching is unaffected.
type ProgressRouter struct {
	parser     *ProgressParser
	client     *Client
	jobID      string
	underlying StderrHandler
	lastSend   time.Time
	sendDelay  time.Duration
}

// NewProgressRouter creates a new ProgressRouter.
// underlying is the handler that receives all stderr lines (typically a StderrBatcher).
func NewProgressRouter(client *Client, jobID string, underlying StderrHandler) *ProgressRouter {
	return &ProgressRouter{
		parser:     NewProgressParser(),
		client:     client,
		jobID:      jobID,
		underlying: underlying,
		sendDelay:  1 * time.Second, // throttle progress updates to once per second
	}
}

// SetDuration sets the known total duration (in microseconds) for more accurate progress.
func (r *ProgressRouter) SetDuration(durationUs int64) {
	r.parser.SetDuration(durationUs)
}

// Handler returns a StderrHandler-compatible function.
func (r *ProgressRouter) Handler() StderrHandler {
	return func(line string) {
		// Always forward the raw line to the underlying handler
		if r.underlying != nil {
			r.underlying(line)
		}

		// Try to extract Duration: from header lines
		if dur := ParseDurationLine(line); dur > 0 {
			r.parser.SetDuration(dur)
		}

		// Only process progress lines
		if !FilterProgressLine(line) {
			return
		}

		// Parse the progress line
		frame := r.parser.ParseLine(line)
		if frame == nil {
			return
		}

		// Throttle: only send updates at the configured interval
		now := time.Now()
		if now.Sub(r.lastSend) < r.sendDelay {
			return
		}
		r.lastSend = now

		// Only send if we have meaningful progress
		if frame.Percent < 0 {
			return
		}

		// Send progress update to server
		if err := r.client.SendProgress(r.jobID, frame.Percent, frame.EtaSeconds, frame.TimeUs, frame.DurationUs, frame.Speed); err != nil {
			log.Printf("Job %s: failed to send progress update: %v", r.jobID, err)
		}
	}
}

// Reset resets the parser state for a new job.
func (r *ProgressRouter) Reset() {
	r.parser.Reset()
	r.lastSend = time.Time{}
}

// SendProgress sends a progress update to the server without changing job status.
func (c *Client) SendProgress(jobID string, progressPercent float64, etaSeconds int, timeUs, durationUs int64, speed float64) error {
	req := protocol.JobUpdateRequest{
		Progress:   progressPercent,
		EtaSeconds: etaSeconds,
		TimeUs:     timeUs,
		DurationUs: durationUs,
		Speed:      speed,
		WorkerID:   c.workerID,
	}

	resp, err := c.doRequest("PATCH", "/jobs/"+jobID, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
