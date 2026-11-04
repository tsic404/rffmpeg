package gpu

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNVIDIASMIProbeTimesOut covers a wedged driver: the probe must return
// instead of holding Worker startup open, and report no device so capability
// detection falls back to the manual list.
func TestNVIDIASMIProbeTimesOut(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "nvidia-smi")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDetector()
	d.nvidiaSMITimeout = 100 * time.Millisecond

	start := time.Now()
	name, driver := d.getNVIDIASMIInfo()
	elapsed := time.Since(start)

	if name != "" || driver != "" {
		t.Errorf("timed-out probe returned name=%q driver=%q, want empty", name, driver)
	}
	if elapsed > 2*time.Second {
		t.Errorf("probe took %v, want it bounded by the 100ms timeout", elapsed)
	}
}

// TestNVIDIASMIProbeKillsForkedChildren covers a wrapper that forks a child
// holding stdout: killing only the direct child leaves the pipe open, so the
// probe must kill the whole process group to stay bounded.
func TestNVIDIASMIProbeKillsForkedChildren(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "nvidia-smi")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 10 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDetector()
	d.nvidiaSMITimeout = 100 * time.Millisecond

	start := time.Now()
	name, driver := d.getNVIDIASMIInfo()
	elapsed := time.Since(start)

	if name != "" || driver != "" {
		t.Errorf("timed-out probe returned name=%q driver=%q, want empty", name, driver)
	}
	if elapsed > 2*time.Second {
		t.Errorf("probe took %v; a forked child kept the output pipe open", elapsed)
	}
}

// TestNVIDIASMIProbeReadsFirstGPU covers the success path: the first line of
// the CSV output is the device name and the driver version.
func TestNVIDIASMIProbeReadsFirstGPU(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "nvidia-smi")
	body := `#!/bin/sh
case "$1" in
--query-gpu=name*) printf 'RTX 4090\nTesla T4\n' ;;
--query-gpu=driver_version*) printf '550.54.14\n' ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDetector()
	name, driver := d.getNVIDIASMIInfo()
	if name != "RTX 4090" {
		t.Errorf("name = %q, want first GPU line", name)
	}
	if driver != "550.54.14" {
		t.Errorf("driver = %q, want 550.54.14", driver)
	}
}
