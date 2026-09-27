// Package accesslog writes the server's HTTP access log without coupling
// response latency to the log destination.
//
// chi's Logger writes the access line from inside the handler chain, before the
// response is flushed: a blocked write there (full pipe, filesystem under
// memory or I/O pressure) delays every in-flight response it logs, health
// probes included. Sink queues lines and drops them once the destination
// stalls, so no request ever waits for the log.
package accesslog

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// DefaultQueueDepth is how many access log lines a Sink buffers before it
// starts dropping. It is deep enough to absorb a burst (every concurrent
// request logs one line) while staying bounded, so a permanently stalled
// destination cannot grow memory without limit.
const DefaultQueueDepth = 4096

// closeDrainTimeout bounds how long Close waits for queued lines to reach a
// stalled destination. A var so tests can shorten it.
var closeDrainTimeout = 2 * time.Second

// Sink is an access log destination that never blocks its caller: lines are
// queued and flushed by a background goroutine, and lines the destination loses
// — dropped from a full queue, or rejected by a failing write — are counted and
// reported.
type Sink struct {
	out           io.Writer
	lines         chan []byte
	stop          chan struct{}
	writerDone    chan struct{}
	stopping      atomic.Bool
	dropped       atomic.Uint64
	writeFailures atomic.Uint64
}

// NewSink starts a Sink writing to out, buffering up to queueDepth lines
// (DefaultQueueDepth when queueDepth <= 0).
func NewSink(out io.Writer, queueDepth int) *Sink {
	if queueDepth <= 0 {
		queueDepth = DefaultQueueDepth
	}
	s := &Sink{
		out:        out,
		lines:      make(chan []byte, queueDepth),
		stop:       make(chan struct{}),
		writerDone: make(chan struct{}),
	}
	go s.drain()
	return s
}

// Write queues p and returns immediately — it never blocks and never fails, so
// the response it logs is never gated by the destination. Lines arriving once
// the queue is full are dropped and counted.
func (s *Sink) Write(p []byte) (int, error) {
	if !s.stopping.Load() {
		line := make([]byte, len(p))
		copy(line, p)
		select {
		case s.lines <- line:
		default:
			s.dropped.Add(1)
		}
	}
	return len(p), nil
}

// Dropped reports how many lines have been discarded because the queue was
// full.
func (s *Sink) Dropped() uint64 {
	return s.dropped.Load()
}

// Close stops accepting lines, flushes what is still queued and returns. A
// destination that is still stalled must not hold shutdown hostage, so Close
// gives up after closeDrainTimeout — leaving the drain goroutine parked in its
// write until the process exits, which is the price of never blocking a caller
// on the destination.
func (s *Sink) Close() {
	if s.stopping.Swap(true) {
		return
	}
	close(s.stop)
	select {
	case <-s.writerDone:
	case <-time.After(closeDrainTimeout):
	}
}

// drain flushes queued lines in arrival order until Close, then flushes the
// remainder.
func (s *Sink) drain() {
	defer close(s.writerDone)
	for {
		select {
		case line := <-s.lines:
			s.emit(line)
		case <-s.stop:
			for {
				select {
				case line := <-s.lines:
					s.emit(line)
				default:
					s.reportLosses()
					return
				}
			}
		}
	}
}

// emit writes one line, preceded by a notice when earlier lines were lost: the
// notice travels the same path the lost lines would have, so it appears exactly
// when the destination starts accepting writes again.
func (s *Sink) emit(line []byte) {
	s.reportLosses()
	if _, err := s.out.Write(line); err != nil {
		// A failing destination (stdout on a full disk) loses the line the same
		// way a full queue does; counting it keeps the loss visible instead of
		// silently shrinking the log.
		s.writeFailures.Add(1)
	}
}

// reportLosses writes the accumulated loss notice and clears what the notice
// accounted for. Counters are cleared only after the notice reached the
// destination, so a destination that is still failing keeps the count for a
// later notice instead of swallowing it; a destination that never accepts
// another write cannot report anything, by construction.
func (s *Sink) reportLosses() {
	dropped, failures := s.dropped.Load(), s.writeFailures.Load()
	if dropped == 0 && failures == 0 {
		return
	}
	notice := fmt.Sprintf("%s [accesslog] lost %d access log line(s) (%d dropped from a full queue, %d write errors)\n",
		time.Now().Format("2006/01/02 15:04:05"), dropped+failures, dropped, failures)
	if _, err := s.out.Write([]byte(notice)); err != nil {
		return
	}
	s.dropped.CompareAndSwap(dropped, 0)
	s.writeFailures.CompareAndSwap(failures, 0)
}

// Middleware returns the chi access log middleware writing through sink. It is
// middleware.Logger with the destination replaced: the line is queued instead of
// written on the response path. NoColor mirrors chi's own default, so the line
// format stays byte-identical to the middleware it replaces.
func Middleware(sink *Sink) func(http.Handler) http.Handler {
	return middleware.RequestLogger(&middleware.DefaultLogFormatter{
		Logger:  log.New(sink, "", log.LstdFlags),
		NoColor: runtime.GOOS == "windows",
	})
}
