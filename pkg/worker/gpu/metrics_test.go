package gpu

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSampleIntelMetrics_JSONParsesSumsEngines verifies that a well-formed
// intel_gpu_top -J -n 1 JSON blob is parsed and engine busy percentages are
// summed.
func TestSampleIntelMetrics_JSONParsesSumsEngines(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "intel_gpu_top")
	// Real intel_gpu_top -J wraps samples in a top-level array: [{ ... }].
	script := `#!/bin/sh
cat <<'EOF'
[{
	"period": {
		"duration": 1000.0,
		"unit": "ms"
	},
	"frequency": {
		"requested": 300.0,
		"actual": 300.0,
		"unit": "MHz"
	},
	"rc6": {
		"value": 50.0,
		"unit": "%"
	},
	"engines": {
		"Render/3D/0": {
			"busy": 42.5,
			"sema": 0.0,
			"wait": 0.0,
			"unit": "%"
		},
		"Blitter/0": {
			"busy": 7.5,
			"sema": 0.0,
			"wait": 0.0,
			"unit": "%"
		},
		"Video/0": {
			"busy": 10.0,
			"sema": 0.0,
			"wait": 0.0,
			"unit": "%"
		}
	}
}]
EOF
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDetector()
	m, ok := d.sampleIntelMetrics()
	if !ok {
		t.Fatal("sampleIntelMetrics returned ok=false for valid JSON")
	}
	if !m.Valid {
		t.Fatal("Metrics.Valid should be true")
	}
	// 42.5 + 7.5 + 10.0 = 60.0
	if got, want := m.UtilPct, 60.0; got != want {
		t.Errorf("UtilPct = %v, want %v", got, want)
	}
	if m.MemUsedMB != 0 {
		t.Errorf("MemUsedMB = %v, want 0 (intel_gpu_top does not report memory)", m.MemUsedMB)
	}
}

// TestSampleIntelMetrics_TruncatedJSONAutoCloses verifies that output
// truncated mid-object (as when the process is killed by the context
// timeout) is rejected, not silently mis-parsed.
func TestSampleIntelMetrics_TruncatedJSONAutoCloses(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "intel_gpu_top")
	// Real truncation: array open, object open, but inner object unterminated.
	// The auto-close appends "}" after stripping "[" and "]", but the inner
	// "Video/0" object is still unterminated → json.Unmarshal fails → ok=false.
	script := `#!/bin/sh
cat <<'EOF'
[{
	"engines": {
		"Render/3D/0": {
			"busy": 25.0,
			"sema": 0.0,
			"wait": 0.0,
			"unit": "%"
EOF
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDetector()
	m, ok := d.sampleIntelMetrics()
	// The auto-close appends "}" but the inner object is still unterminated,
	// so json.Unmarshal should fail → ok=false. This is acceptable: a
	// genuinely broken output is rejected, not silently mis-parsed.
	if ok {
		t.Fatalf("sampleIntelMetrics returned ok=true for truncated JSON; Metrics=%+v", m)
	}
}

// TestSampleIntelMetrics_MissingClosingBracket verifies the manpage scenario:
// "JSON output will be correctly terminated when the tool cleanly exits,
// otherwise one square bracket needs to be added before parsing." When the
// process is killed after emitting the object but before the closing "]",
// the output is [{ ... } (no closing bracket). The code must still parse it.
func TestSampleIntelMetrics_MissingClosingBracket(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "intel_gpu_top")
	// Array open, complete object, but no closing "]".
	script := `#!/bin/sh
cat <<'EOF'
[{
	"engines": {
		"Render/3D/0": {
			"busy": 33.0,
			"sema": 0.0,
			"wait": 0.0,
			"unit": "%"
		}
	}
}
EOF
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDetector()
	m, ok := d.sampleIntelMetrics()
	if !ok {
		t.Fatal("sampleIntelMetrics returned ok=false for array without closing bracket")
	}
	if !m.Valid {
		t.Fatal("Metrics.Valid should be true")
	}
	if got, want := m.UtilPct, 33.0; got != want {
		t.Errorf("UtilPct = %v, want %v", got, want)
	}
}

// TestSampleIntelMetrics_NotInstalled verifies that missing intel_gpu_top
// returns ok=false without error.
func TestSampleIntelMetrics_NotInstalled(t *testing.T) {
	// Ensure intel_gpu_top is not on PATH.
	t.Setenv("PATH", "/nonexistent")
	d := NewDetector()
	_, ok := d.sampleIntelMetrics()
	if ok {
		t.Fatal("sampleIntelMetrics returned ok=true with no intel_gpu_top")
	}
}

// TestSampleIntelMetrics_HangDoesNotBlock verifies that a hung intel_gpu_top
// process is killed by the context timeout and SampleMetrics returns promptly.
func TestSampleIntelMetrics_HangDoesNotBlock(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "intel_gpu_top")
	script := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDetector()
	done := make(chan struct{})
	go func() {
		d.sampleIntelMetrics()
		close(done)
	}()

	select {
	case <-done:
		// returned within the command timeout — good
	case <-time.After(10 * time.Second):
		t.Fatal("sampleIntelMetrics still blocked 10s with a hung intel_gpu_top")
	}
}

// writeSysfsTree creates a fake amdgpu sysfs tree under a temp dir and
// returns a Detector whose sysfsRoot points at it. Each card entry is a
// map of filename→content; omitting a file simulates ENOTSUP/missing.
func writeSysfsTree(t *testing.T, cards map[string]map[string]string) *Detector {
	t.Helper()
	root := t.TempDir()
	drmDir := filepath.Join(root, "class/drm")
	if err := os.MkdirAll(drmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for cardName, files := range cards {
		cardDir := filepath.Join(drmDir, cardName, "device")
		if err := os.MkdirAll(cardDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for fname, content := range files {
			if err := os.WriteFile(filepath.Join(cardDir, fname), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &Detector{sysfsRoot: root}
}

const amdgpuUevent = "MAJOR=226\nMINOR=128\nOF_FULLNAME=/proto/soc/firmware\nDRIVER=amdgpu\n"

// TestSampleAMDMetrics_MultiCardSum verifies that amdgpu sysfs files are
// read and summed across multiple AMD cards.
func TestSampleAMDMetrics_MultiCardSum(t *testing.T) {
	d := writeSysfsTree(t, map[string]map[string]string{
		"card0": {
			"uevent":             amdgpuUevent,
			"gpu_busy_percent":   "42\n",
			"mem_info_vram_used": "1073741824\n", // 1 GiB
		},
		"card1": {
			"uevent":             amdgpuUevent,
			"gpu_busy_percent":   "58\n",
			"mem_info_vram_used": "536870912\n", // 512 MiB
		},
	})
	m, ok := d.sampleAMDMetrics()
	if !ok {
		t.Fatal("sampleAMDMetrics returned ok=false for valid sysfs tree")
	}
	if !m.Valid {
		t.Error("Metrics.Valid should be true")
	}
	if got, want := m.UtilPct, 100.0; got != want {
		t.Errorf("UtilPct = %v, want %v (42+58)", got, want)
	}
	if got, want := m.MemUsedMB, 1536; got != want {
		t.Errorf("MemUsedMB = %v, want %v (1024+512)", got, want)
	}
}

// TestSampleAMDMetrics_GpuBusyMissingDoesNotReportValid verifies the
// blocker scenario from review: when mem_info_vram_used is readable but
// gpu_busy_percent is missing (ENOTSUP on some ASICs), the sample must NOT
// be reported as Valid — otherwise consumers interpret UtilPct=0 as a
// real 0% reading instead of "no sample available".
func TestSampleAMDMetrics_GpuBusyMissingDoesNotReportValid(t *testing.T) {
	d := writeSysfsTree(t, map[string]map[string]string{
		"card0": {
			"uevent":             amdgpuUevent,
			"mem_info_vram_used": "7340032\n", // 7 MiB
			// gpu_busy_percent intentionally absent
		},
	})
	m, ok := d.sampleAMDMetrics()
	if ok {
		t.Fatalf("sampleAMDMetrics returned ok=true when gpu_busy_percent is missing; Metrics=%+v", m)
	}
	if m.Valid {
		t.Error("Metrics.Valid should be false when gpu_busy_percent is unreadable")
	}
	if m.UtilPct != 0 {
		t.Errorf("UtilPct = %v, want 0", m.UtilPct)
	}
	// MemUsedMB may still be populated from the readable file, but Valid
	// governs whether consumers should trust it.
	if m.MemUsedMB != 7 {
		t.Errorf("MemUsedMB = %v, want 7", m.MemUsedMB)
	}
}

// TestSampleAMDMetrics_NonAMDCardSkipped verifies that non-amdgpu cards
// (e.g. Intel i915) in the same /sys/class/drm are skipped.
func TestSampleAMDMetrics_NonAMDCardSkipped(t *testing.T) {
	d := writeSysfsTree(t, map[string]map[string]string{
		"card0": {
			"uevent":             "MAJOR=226\nMINOR=128\nDRIVER=i915\n",
			"gpu_busy_percent":   "99\n",
			"mem_info_vram_used": "1048576\n",
		},
	})
	m, ok := d.sampleAMDMetrics()
	if ok {
		t.Fatalf("sampleAMDMetrics returned ok=true for non-AMD card; Metrics=%+v", m)
	}
}

// TestSampleAMDMetrics_ConnectorSymlinkNotGlobbed verifies that DRM
// connector entries (card0-DP-1, etc.) are not matched by the card glob.
func TestSampleAMDMetrics_ConnectorSymlinkNotGlobbed(t *testing.T) {
	d := writeSysfsTree(t, map[string]map[string]string{
		"card0": {
			"uevent":             amdgpuUevent,
			"gpu_busy_percent":   "50\n",
			"mem_info_vram_used": "0\n",
		},
		// Connector entry — should not be globbed by card[0-9]
		"card0-DP-1": {
			"uevent": "MAJOR=226\nMINOR=128\nDRIVER=amdgpu\n",
		},
	})
	m, ok := d.sampleAMDMetrics()
	if !ok {
		t.Fatal("sampleAMDMetrics returned ok=false")
	}
	// Only card0's gpu_busy_percent (50) should be summed; connector has
	// no gpu_busy_percent and should not have been visited at all.
	if got, want := m.UtilPct, 50.0; got != want {
		t.Errorf("UtilPct = %v, want %v (connector should not contribute)", got, want)
	}
}

// TestSampleAMDMetrics_RealSysfsSmoke is a smoke test against the real
// /sys tree. On non-AMD machines it returns ok=false; on AMD machines it
// verifies Valid and non-negative values.
func TestSampleAMDMetrics_RealSysfsSmoke(t *testing.T) {
	d := NewDetector()
	m, ok := d.sampleAMDMetrics()
	if ok {
		if !m.Valid {
			t.Error("Metrics.Valid should be true when ok is true")
		}
		if m.UtilPct < 0 {
			t.Errorf("UtilPct = %v, should be >= 0", m.UtilPct)
		}
	}
}

// TestReadSysfsInt_ParsesValue verifies the sysfs integer reader.
func TestReadSysfsInt_ParsesValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpu_busy_percent")
	if err := os.WriteFile(path, []byte("73\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readSysfsInt(path); got != 73 {
		t.Errorf("readSysfsInt = %d, want 73", got)
	}
}

// TestReadSysfsInt_MissingFile returns -1 for a non-existent file.
func TestReadSysfsInt_MissingFile(t *testing.T) {
	if got := readSysfsInt("/nonexistent/path/12345"); got != -1 {
		t.Errorf("readSysfsInt = %d, want -1 for missing file", got)
	}
}

// TestReadSysfsInt_Garbage returns -1 for unparseable content.
func TestReadSysfsInt_Garbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad")
	if err := os.WriteFile(path, []byte("not-a-number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readSysfsInt(path); got != -1 {
		t.Errorf("readSysfsInt = %d, want -1 for garbage", got)
	}
}

// TestReadSysfsInt64_ParsesBytes verifies the int64 sysfs reader for byte
// counts (e.g. mem_info_vram_used).
func TestReadSysfsInt64_ParsesBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mem_info_vram_used")
	// 512 MiB in bytes
	val := int64(512 * 1024 * 1024)
	if err := os.WriteFile(path, []byte(formatInt64(val)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readSysfsInt64(path); got != val {
		t.Errorf("readSysfsInt64 = %d, want %d", got, val)
	}
}

// TestReadSysfsInt64_MissingFile returns -1.
func TestReadSysfsInt64_MissingFile(t *testing.T) {
	if got := readSysfsInt64("/nonexistent/path/67890"); got != -1 {
		t.Errorf("readSysfsInt64 = %d, want -1", got)
	}
}

// TestSampleMetrics_FallbackChain verifies that when nvidia-smi is absent and
// intel_gpu_top is present, the Intel path is used. This exercises the
// fallback wiring in SampleMetrics without requiring real hardware.
func TestSampleMetrics_FallbackChainIntel(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	// nvidia-smi absent (unless real one is installed — then skip, because
	// the NVIDIA path would shadow the fallback).
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		t.Skip("real nvidia-smi installed — would shadow Intel fallback")
	}

	dir := t.TempDir()
	fake := filepath.Join(dir, "intel_gpu_top")
	// Real intel_gpu_top -J wraps samples in a top-level array.
	script := `#!/bin/sh
cat <<'EOF'
[{
	"engines": {
		"Render/3D/0": {
			"busy": 99.0,
			"sema": 0.0,
			"wait": 0.0,
			"unit": "%"
		}
	}
}]
EOF
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDetector()
	m := d.SampleMetrics()
	if !m.Valid {
		t.Fatal("SampleMetrics returned invalid — Intel fallback not used")
	}
	if got, want := m.UtilPct, 99.0; got != want {
		t.Errorf("UtilPct = %v, want %v", got, want)
	}
}

// TestSampleMetrics_AllAbsentReturnsInvalid verifies that when no GPU tool
// is available and no amdgpu sysfs entries exist, SampleMetrics returns
// invalid Metrics. On machines with amdgpu sysfs, the AMD path succeeds and
// the test is skipped.
func TestSampleMetrics_AllAbsentReturnsInvalid(t *testing.T) {
	// Remove all GPU tools from PATH.
	t.Setenv("PATH", "/nonexistent")
	d := NewDetector()

	// If amdgpu sysfs is present, the AMD path will succeed — skip.
	if m, ok := d.sampleAMDMetrics(); ok {
		t.Skipf("amdgpu sysfs present on this machine (Metrics=%+v) — cannot test all-absent", m)
	}

	m := d.SampleMetrics()
	if m.Valid {
		t.Fatalf("SampleMetrics returned valid with no GPU tools; Metrics=%+v", m)
	}
}

// formatInt64 converts an int64 to its decimal string representation without
// importing strconv (test readability helper).
func formatInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
