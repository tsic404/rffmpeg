package client

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWSClient_LargeStdoutLineNoSpuriousGap is the TSI-2420 regression test.
//
// Failure mode (short streaming-output jobs, e.g. `-f mp4 -` finishing in
// ~1s): the worker's StdoutBatcher concatenates up to 10×32KB executor
// chunks and base64-encodes them into a single WSMsgStdout message — a JSON
// line of several hundred KB. The client listen loop split frames with a
// bufio.Scanner at its DEFAULT 64KB token limit and never checked
// scanner.Err(), so the oversized line aborted the scan silently and every
// later line in the frame was dropped. The next delivered message then tripped
// the gap detector: "WebSocket stream gap detected ... expected seq N, got
// N+1" — failing an intact job with exit code 1 even though nothing was lost
// on the wire.
//
// The fix grows the scanner buffer to wsMaxFrameLineBytes (1MB, matching the
// connection read limit) and logs scanner.Err() instead of swallowing it.
// This test replays exactly that wire shape: huge stdout lines interleaved
// with sequenced stderr in one frame, then asserts no gap is flagged and all
// payloads arrive.
func TestWSClient_LargeStdoutLineNoSpuriousGap(t *testing.T) {
	// 300KB of base64 payload: far above bufio's default 64KB token limit,
	// below the 1MB connection read limit — the exact production shape of a
	// full StdoutBatcher flush for streaming mp4 output.
	bigPayload := strings.Repeat("QUJD", 75*1024) // 300KB

	msgs := []string{
		fmt.Sprintf(`{"type":"stdout","job_id":"job-1","payload":"%s","seq":1}`, bigPayload),
		`{"type":"stderr","job_id":"job-1","payload":"frame= 12 fps=0.0","seq":2}`,
		fmt.Sprintf(`{"type":"stdout","job_id":"job-1","payload":"%s","seq":3}`, bigPayload),
	}
	srv := wsGapTestServer(t, msgs, nil)
	defer srv.Close()

	var stdoutBytes atomic.Int64
	var stderrChunks atomic.Int64
	c := NewWSClient(srv.URL, "job-1", "",
		WithOnStdout(func(chunk []byte) { stdoutBytes.Add(int64(len(chunk))) }),
		WithOnStderr(func(chunk string) { stderrChunks.Add(1) }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	listenDone := make(chan struct{})
	go func() {
		c.Listen(ctx)
		close(listenDone)
	}()

	deadline := time.After(2 * time.Second)
	for stdoutBytes.Load() < int64(len(bigPayload)) {
		select {
		case <-listenDone:
			// Server holds the frame open briefly before closing; Listen
			// returning means everything deliverable has been delivered.
		case <-deadline:
			t.Fatalf("timed out waiting for stdout delivery: got %d/%d bytes",
				stdoutBytes.Load(), len(bigPayload))
		}
		if stdoutBytes.Load() >= int64(len(bigPayload)) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give the trailing stderr + second stdout a moment to be processed.
	time.Sleep(200 * time.Millisecond)
	c.Close()

	if c.HasGap() {
		t.Error("oversized-but-delivered stdout line must not trigger gap detection; " +
			"scanner likely aborted with ErrTooLong and dropped sequenced messages")
	}
	// The onStdout handler receives DECODED bytes: base64 inflates by 4/3,
	// so each 300KB payload decodes to 230400 bytes on the handler side.
	wantStdoutBytes := int64(2 * len(bigPayload) * 3 / 4)
	if got := stdoutBytes.Load(); got != wantStdoutBytes {
		t.Errorf("expected exactly %d decoded stdout bytes (both chunks), got %d",
			wantStdoutBytes, got)
	}
	if got := stderrChunks.Load(); got == 0 {
		t.Error("stderr chunk after the oversized stdout line was dropped — scanner aborted mid-frame")
	}
}
