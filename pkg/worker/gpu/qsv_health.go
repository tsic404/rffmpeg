package gpu

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// QSVHealthTimeout is the maximum time to wait for the QSV health probe.
const QSVHealthTimeout = 5 * time.Second

// CheckQSVHealth verifies that the Intel QSV (Quick Sync Video) MFX runtime is
// functional on the given render device. It creates and destroys a QSV hardware
// device context via ffmpeg to confirm that the Intel Media SDK / oneVPL
// hardware implementation (libmfxhw64.so) is available and working.
//
// On systems where intel-media-sdk or vpl-gpu-rt is not installed, this
// check will return false. In that case, rffmpeg should prefer VA-API
// encoders (h264_vaapi, etc.) which rely only on intel-media-driver.
func CheckQSVHealth(renderDevicePath string) (healthy bool, errMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), QSVHealthTimeout)
	defer cancel()

	// Build a minimal ffmpeg command that only initializes a QSV device:
	//   ffmpeg -y -init_hw_device qsv=hw,child_device=<path> -f lavfi -i nullsrc -frames:v 1 -f null -
	// This initializes the MFX session and immediately exits without encoding.
	args := []string{
		"-y",
		"-init_hw_device", "qsv=hw,child_device=" + renderDevicePath,
		"-f", "lavfi",
		"-i", "nullsrc",
		"-frames:v", "1",
		"-f", "null",
		"-",
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	// Pdeathsig reaps ffmpeg if the worker dies while a QSV health probe
	// is in flight (TSI-2476).
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, ""
	}

	stderrStr := stderr.String()
	if strings.Contains(stderrStr, "MFX session: -9") ||
		strings.Contains(stderrStr, "MFX_ERR_DEVICE_FAILED") ||
		strings.Contains(stderrStr, "Device creation failed") {
		return false, "QSV MFX session creation failed (-9): Intel Media SDK runtime (intel-media-sdk) may not be installed"
	}

	return false, stderrStr
}
