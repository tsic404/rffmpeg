package workerhealth

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ThroughputScaleEntry is one fault-injection rule: heartbeats from a worker
// report their throughput multiplied by Factor. It exists so the slow-worker
// eviction path can be driven end to end without actually slowing a worker
// down — a genuinely slow encoder needs enough completed jobs on that worker
// to clear the warmup gate and several EWMA samples to converge, which no
// short E2E window can guarantee.
type ThroughputScaleEntry struct {
	// Worker matches a worker's registered ID or name.
	Worker string
	// Factor multiplies the reported jobs/sec; values below 1 stage a slow node.
	Factor float64
	// Window bounds how long the rule applies, measured from its first
	// non-zero throughput sample (0 = for the server's lifetime). A bounded
	// window is what makes the recovery half of the eviction scenario
	// reachable: once the rule lapses the worker's real throughput flows again
	// and its EWMA climbs back above median/RecoveryThreshold.
	Window time.Duration
}

// ParseThroughputScale parses the RFFMPEG_THROUGHPUT_SCALE value:
// comma-separated "<worker>=<factor>[@<window>]" entries, where <worker> is a
// worker ID or name, <factor> a multiplier greater than 0, and <window> an
// optional Go duration. Any malformed entry rejects the whole value: a
// partially applied fault injection would leave an E2E run waiting on an
// eviction that can never happen.
func ParseThroughputScale(raw string) ([]ThroughputScaleEntry, error) {
	var entries []ThroughputScaleEntry
	seen := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		worker, spec, ok := strings.Cut(item, "=")
		worker = strings.TrimSpace(worker)
		if !ok || worker == "" {
			return nil, fmt.Errorf("entry %q: want <worker>=<factor>[@<window>]", item)
		}
		if seen[worker] {
			return nil, fmt.Errorf("entry %q: worker %q listed twice", item, worker)
		}
		factorSpec, windowSpec, hasWindow := strings.Cut(spec, "@")
		factor, err := strconv.ParseFloat(strings.TrimSpace(factorSpec), 64)
		// ParseFloat accepts "NaN"/"Inf"/"Infinity", which the positivity check
		// cannot reject: a non-finite factor poisons the EWMA (NaN never crosses
		// the threshold, +Inf evicts every peer) instead of failing startup.
		if err != nil || math.IsNaN(factor) || math.IsInf(factor, 0) || factor <= 0 {
			return nil, fmt.Errorf("entry %q: factor %q must be a finite number greater than 0", item, factorSpec)
		}
		var window time.Duration
		if hasWindow {
			window, err = time.ParseDuration(strings.TrimSpace(windowSpec))
			if err != nil || window <= 0 {
				return nil, fmt.Errorf("entry %q: window %q must be a positive duration", item, windowSpec)
			}
		}
		seen[worker] = true
		entries = append(entries, ThroughputScaleEntry{Worker: worker, Factor: factor, Window: window})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no entries in %q", raw)
	}
	return entries, nil
}

// ThroughputScaler applies ThroughputScaleEntry rules to heartbeat throughput.
// A nil scaler, or one built from no entries, is a no-op.
type ThroughputScaler struct {
	mu    sync.Mutex
	rules []throughputScaleRule
}

type throughputScaleRule struct {
	entry     ThroughputScaleEntry
	startedAt time.Time // set when the rule first matches; zero until then
	expired   bool      // window elapsed: the worker reports real throughput again
}

// NewThroughputScaler builds a scaler from parsed rules.
func NewThroughputScaler(entries []ThroughputScaleEntry) *ThroughputScaler {
	scaler := &ThroughputScaler{rules: make([]throughputScaleRule, 0, len(entries))}
	for _, entry := range entries {
		scaler.rules = append(scaler.rules, throughputScaleRule{entry: entry})
	}
	return scaler
}

// Enabled reports whether any rule is configured. Safe on a nil scaler.
func (s *ThroughputScaler) Enabled() bool {
	return s != nil && len(s.rules) > 0
}

// Scale returns the throughput a heartbeat from this worker must be recorded
// with: the reported value multiplied by the matching rule's factor, or the
// reported value unchanged when no rule matches. Safe on a nil scaler.
func (s *ThroughputScaler) Scale(workerID, workerName string, jobsPerSec float64) float64 {
	if !s.Enabled() {
		return jobsPerSec
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rule := s.matchLocked(workerID, workerName)
	if rule == nil {
		return jobsPerSec
	}
	now := time.Now()
	if rule.startedAt.IsZero() {
		// The window clock starts at the first sample that actually carries
		// throughput: a worker's boot heartbeats report 0 jobs/sec and must not
		// consume the window while the E2E run is still setting up its load.
		if jobsPerSec <= 0 {
			return jobsPerSec * rule.entry.Factor
		}
		rule.startedAt = now
		log.Printf("Throughput scale injection active for worker %q: reporting %.4f jobs/sec (factor %g, window %s)",
			rule.entry.Worker, jobsPerSec*rule.entry.Factor, rule.entry.Factor, windowLabel(rule.entry.Window))
	} else if rule.entry.Window > 0 && now.Sub(rule.startedAt) >= rule.entry.Window {
		rule.expired = true
		log.Printf("Throughput scale injection ended for worker %q after %s: reporting real throughput again",
			rule.entry.Worker, rule.entry.Window)
		return jobsPerSec
	}
	return jobsPerSec * rule.entry.Factor
}

// matchLocked returns the rule targeting this worker, or nil. Caller holds mu.
func (s *ThroughputScaler) matchLocked(workerID, workerName string) *throughputScaleRule {
	for i := range s.rules {
		if s.rules[i].expired {
			continue
		}
		matchesID := s.rules[i].entry.Worker == workerID
		matchesName := workerName != "" && s.rules[i].entry.Worker == workerName
		if matchesID || matchesName {
			return &s.rules[i]
		}
	}
	return nil
}

func windowLabel(window time.Duration) string {
	if window <= 0 {
		return "server lifetime"
	}
	return window.String()
}
