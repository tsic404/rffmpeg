package gpu

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// metricCmdTimeout bounds every external metrics query so a driver wedge
// never blocks the heartbeat loop (which also carries job cancellation).
const metricCmdTimeout = 3 * time.Second

// Metrics holds point-in-time GPU utilization metrics.
// On multi-GPU hosts the values are summed across all GPUs, so UtilPct is an
// aggregate busyness (two GPUs at 50% report 100), not a per-device average.
// Valid reports whether the sample came from a successful query of any source
// (nvidia-smi, intel_gpu_top, or amdgpu sysfs); a valid sample with UtilPct
// == 0 is a real 0% reading, not "unavailable".
type Metrics struct {
	UtilPct   float64 // Summed GPU utilization percentage across all GPUs (0-100*N)
	MemUsedMB int     // Total GPU memory in use across all GPUs, megabytes
	Valid     bool    // True when sampled successfully; zero Metrics means unavailable
}

// SampleMetrics queries GPU utilization, trying NVIDIA first, then Intel
// QSV/VAAPI (intel_gpu_top), then AMD (amdgpu sysfs). Returns zero Metrics
// when no source is available — workers without a supported GPU (or the
// required tools/sysfs) simply report no GPU metrics.
func (d *Detector) SampleMetrics() Metrics {
	if m, ok := d.sampleNVIDIAMetrics(); ok {
		return m
	}
	if m, ok := d.sampleIntelMetrics(); ok {
		return m
	}
	if m, ok := d.sampleAMDMetrics(); ok {
		return m
	}
	return Metrics{}
}

// sampleNVIDIAMetrics runs a single nvidia-smi invocation for all GPUs and
// sums utilization/memory across them. ok is false if nvidia-smi is missing
// or the query fails.
func (d *Detector) sampleNVIDIAMetrics() (m Metrics, ok bool) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return Metrics{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), metricCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=utilization.gpu,memory.used",
		"--format=csv,noheader,nounits")
	// Kill the whole process group: a bare `sleep`-style script forks a
	// child that survives a plain ctx kill and keeps the output pipes open,
	// blocking cmd.Output forever.
	// Pdeathsig ensures the metrics subprocess is reaped if the worker
	// itself dies, preventing orphaned nvidia-smi/intel_gpu_top processes
	// (TSI-2476).
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
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
	m.Valid = true
	return m, true
}

// sampleIntelMetrics queries Intel GPU utilization via intel_gpu_top. It
// runs one JSON sample (-n 1) and sums the "busy" percentage across all
// engine classes (Render, Video, VideoEnhance, Blitter). Memory is not
// reported by intel_gpu_top, so MemUsedMB stays 0. ok is false if
// intel_gpu_top is missing or the query fails.
//
// intel_gpu_top -J emits a top-level JSON array: [{ ...sample... }]. With
// -n 1 the array contains exactly one element. Older versions may omit the
// closing bracket when killed by timeout (per manpage: "JSON output will be
// correctly terminated when the tool cleanly exits, otherwise one square
// bracket needs to be added before parsing"); we handle both cases.
func (d *Detector) sampleIntelMetrics() (m Metrics, ok bool) {
	if _, err := exec.LookPath("intel_gpu_top"); err != nil {
		return Metrics{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), metricCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "intel_gpu_top",
		"-J",      // JSON output
		"-o", "-", // stdout
		"-s", "1000", // 1s refresh
		"-n", "1") // single sample then exit
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	output, err := cmd.Output()
	if err != nil {
		return Metrics{}, false
	}

	// intel_gpu_top -J wraps each sample in a top-level array: [{ ... }].
	// With -n 1 there is exactly one sample object inside. Strip the array
	// wrapper so we can unmarshal a single object; if the process was
	// killed before emitting the closing bracket, patch it on.
	doc := strings.TrimSpace(string(output))
	doc = strings.TrimPrefix(doc, "[")
	doc = strings.TrimSuffix(doc, "]")
	doc = strings.TrimSpace(doc)
	// If the closing brace is missing (truncated output), append one so
	// json.Unmarshal doesn't fail on a dangling object.
	if !strings.HasSuffix(doc, "}") {
		doc += "}"
	}

	var sample intelTopSample
	if err := json.Unmarshal([]byte(doc), &sample); err != nil {
		return Metrics{}, false
	}

	for _, eng := range sample.Engines {
		m.UtilPct += eng.Busy
	}
	m.Valid = true
	return m, true
}

// sampleAMDMetrics reads amdgpu sysfs files for all AMD cards and sums
// utilization/memory across them. ok is false if no amdgpu sysfs entries
// are found or no card has a readable gpu_busy_percent.
//
// Valid is true only when at least one card's gpu_busy_percent was
// successfully read — a missing gpu_busy_percent (ENOTSUP on some ASICs)
// must not produce Valid=true with UtilPct=0, which consumers would
// interpret as a real 0% reading rather than "no sample".
func (d *Detector) sampleAMDMetrics() (m Metrics, ok bool) {
	drmDir := filepath.Join(d.sysfsRoot, "class/drm")
	cards, err := filepath.Glob(filepath.Join(drmDir, "card[0-9]"))
	if err != nil || len(cards) == 0 {
		return Metrics{}, false
	}

	utilRead := false
	for _, card := range cards {
		devDir := filepath.Join(card, "device")
		uevent, err := os.ReadFile(filepath.Join(devDir, "uevent"))
		if err != nil {
			continue
		}
		// Match DRIVER=amdgpu precisely; a bare "amdgpu" substring could
		// match other uevent fields.
		if !strings.Contains(string(uevent), "DRIVER=amdgpu") {
			continue
		}

		// gpu_busy_percent is the authority for Valid: without it UtilPct
		// is genuinely unknown, not zero.
		if util := readSysfsInt(filepath.Join(devDir, "gpu_busy_percent")); util >= 0 {
			m.UtilPct += float64(util)
			utilRead = true
		}
		if memBytes := readSysfsInt64(filepath.Join(devDir, "mem_info_vram_used")); memBytes >= 0 {
			m.MemUsedMB += int(memBytes / (1024 * 1024))
		}
	}
	m.Valid = utilRead
	return m, utilRead
}

// readSysfsInt reads a sysfs file and returns its integer value. Returns
// -1 if the file cannot be read or parsed.
func readSysfsInt(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return n
}

// readSysfsInt64 reads a sysfs file and returns its int64 value. Returns
// -1 if the file cannot be read or parsed.
func readSysfsInt64(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// intelTopSample mirrors the intel_gpu_top -J -n 1 JSON output. Only the
// fields used for metrics are decoded; power/frequency/imc-bandwidth are
// ignored.
type intelTopSample struct {
	Engines map[string]intelTopEngine `json:"engines"`
}

type intelTopEngine struct {
	Busy float64 `json:"busy"`
	Sema float64 `json:"sema"`
	Wait float64 `json:"wait"`
	Unit string  `json:"unit"`
}
