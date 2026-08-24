package gpu

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Metrics holds point-in-time GPU utilization metrics.
// On multi-GPU hosts the values are summed across all GPUs, so UtilPct is an
// aggregate busyness (two GPUs at 50% report 100), not a per-device average.
// Zero values mean "not available" and are omitted from JSON payloads (see
// the omitempty tags on the heartbeat request).
type Metrics struct {
	UtilPct   float64 // Summed GPU utilization percentage across all GPUs (0-100*N)
	MemUsedMB int     // Total GPU memory in use across all GPUs, megabytes
}

// SampleMetrics queries GPU utilization via nvidia-smi. Returns zero Metrics
// when nvidia-smi is unavailable or fails — workers without NVIDIA GPUs (or
// without the tool installed) simply report no GPU metrics.
func (d *Detector) SampleMetrics() Metrics {
	m, _ := d.sampleNVIDIAMetrics()
	return m
}

// sampleNVIDIAMetrics runs a single nvidia-smi invocation for all GPUs and
// sums utilization/memory across them. ok is false if nvidia-smi is missing
// or the query fails.
func (d *Detector) sampleNVIDIAMetrics() (m Metrics, ok bool) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return Metrics{}, false
	}

	// Bound the query: a hung nvidia-smi (driver wedge) must not block the
	// heartbeat loop, which also carries job cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=utilization.gpu,memory.used",
		"--format=csv,noheader,nounits")
	// Kill the whole process group: a bare `sleep`-style script forks a
	// child that survives a plain ctx kill and keeps the output pipes open,
	// blocking cmd.Output forever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// ctx timeout can fire before Start() sets cmd.Process (start vs
		// cancel race window); dereferencing nil would panic the caller.
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	output, err := cmd.Output()
	if err != nil {
		return Metrics{}, false
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			continue
		}
		if util, err := strconv.ParseFloat(strings.TrimSpace(fields[0]), 64); err == nil {
			m.UtilPct += util
		}
		if mem, err := strconv.Atoi(strings.TrimSpace(fields[1])); err == nil {
			m.MemUsedMB += mem
		}
	}
	return m, true
}
