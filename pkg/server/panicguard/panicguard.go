// Package panicguard converts background-goroutine panics from silent process
// deaths into logged, diagnosable ones.
//
// A panic in any goroutine that is not recovered terminates the whole Go
// process, and the runtime writes the stack trace to os.Stderr — which a
// deployment that only captures stdout (server.log) never sees, so the crash
// looks like a silent exit with no panic/fatal in the log tail. Guard recovers
// the panic just long enough to log the value and full stack through the
// configured logger (which writes to the log file), then re-panics: the process
// still crashes, so a supervisor's restart and recovery behavior is unchanged,
// but the cause is now on disk instead of lost.
package panicguard

import (
	"log"
	"runtime/debug"
)

// Guard runs fn, recovering any panic: it logs the panic value and stack trace,
// then re-panics so the process still exits non-zero. Use it as
// `go panicguard.Guard("name", fn)` for long-lived background loops (hub,
// scheduler, health monitor, per-connection pumps).
func Guard(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("FATAL: %s panicked: %v\n%s", name, r, debug.Stack())
			panic(r)
		}
	}()
	fn()
}
