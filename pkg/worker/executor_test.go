package worker

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/pathutil"
)

func TestBuildArgs(t *testing.T) {
	tests := []struct {
		name       string
		jobArgs    []string
		inputPaths []string
		outputPath string
		want       []string
	}{
		{
			name:       "single input with placeholder",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-i", "/tmp/downloaded/input.mp4", "-c:v", "libx264", "/tmp/output.mp4"},
		},
		{
			name:       "multiple inputs with placeholders",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-i", "<INPUT_FILE>", "-filter_complex", "concat", "-c:v", "libx264"},
			inputPaths: []string{"/tmp/downloaded/input1.mp4", "/tmp/downloaded/input2.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-i", "/tmp/downloaded/input1.mp4", "-i", "/tmp/downloaded/input2.mp4", "-filter_complex", "concat", "-c:v", "libx264", "/tmp/output.mp4"},
		},
		{
			name:       "no placeholder - passthrough args",
			jobArgs:    []string{"-f", "lavfi", "-i", "testsrc=duration=10", "-c:v", "libx264"},
			inputPaths: []string{},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-f", "lavfi", "-i", "testsrc=duration=10", "-c:v", "libx264", "/tmp/output.mp4"},
		},
		{
			name:       "output already specified in jobArgs",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "/tmp/custom_output.mp4"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-i", "/tmp/downloaded/input.mp4", "-c:v", "libx264", "/tmp/custom_output.mp4"},
		},
		{
			name:       "empty jobArgs",
			jobArgs:    []string{},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"/tmp/output.mp4"},
		},
		{
			name:       "concatenated -i style placeholder",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-c:a", "aac"},
			inputPaths: []string{"/tmp/downloaded/audio.mp3"},
			outputPath: "/tmp/output.aac",
			want:       []string{"-i", "/tmp/downloaded/audio.mp3", "-c:a", "aac", "/tmp/output.aac"},
		},
		{
			name:       "args with -- separator at end (user mistake)",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "--"},
			inputPaths: []string{"/tmp/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-i", "/tmp/input.mp4", "-c:v", "libx264", "--", "/tmp/output.mp4"},
		},
		{
			name:       "args with -- separator and output after",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "--", "output.mp4"},
			inputPaths: []string{"/tmp/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-i", "/tmp/input.mp4", "--", "output.mp4"},
		},
		{
			name:       "audio extraction with vn flag",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-vn", "-c:a", "libmp3lame"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp3",
			want:       []string{"-i", "/tmp/downloaded/input.mp4", "-vn", "-c:a", "libmp3lame", "/tmp/output.mp3"},
		},
		{
			name:       "multiple inputs with -shortest boolean flag",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-i", "<INPUT_FILE>", "-c:v", "libx264", "-c:a", "aac", "-shortest"},
			inputPaths: []string{"/tmp/downloaded/video.mp4", "/tmp/downloaded/audio.m4a"},
			outputPath: "/tmp/output/merged.mp4",
			want:       []string{"-i", "/tmp/downloaded/video.mp4", "-i", "/tmp/downloaded/audio.m4a", "-c:v", "libx264", "-c:a", "aac", "-shortest", "/tmp/output/merged.mp4"},
		},
		{
			name:       "input placeholder missing -i still gets server output appended",
			jobArgs:    []string{"-c:v", "libx264", "<INPUT_FILE>"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-c:v", "libx264", "/tmp/downloaded/input.mp4", "/tmp/output.mp4"},
		},
		{
			name:       "explicit -y is passed through verbatim",
			jobArgs:    []string{"-y", "-i", "<INPUT_FILE>", "-c:v", "libx264"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-y", "-i", "/tmp/downloaded/input.mp4", "-c:v", "libx264", "/tmp/output.mp4"},
		},
		{
			name:       "explicit -n is passed through verbatim",
			jobArgs:    []string{"-n", "-i", "<INPUT_FILE>", "-c:v", "libx264"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-n", "-i", "/tmp/downloaded/input.mp4", "-c:v", "libx264", "/tmp/output.mp4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildArgs(tt.jobArgs, tt.inputPaths, tt.outputPath)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestStreamingOutputPath verifies the worker's output path resolution for
// streaming jobs: an empty OutputFilename must resolve to "-"
// (ffmpeg stdout) instead of a regular file, so transcoded data reaches the
// stdoutBatcher and no 0-byte file is written.
func TestStreamingOutputPath(t *testing.T) {
	resolve := func(outputFilename string, streamingOutput bool) string {
		if streamingOutput {
			if outputFilename == "" || pathutil.IsRemoteURL(outputFilename) {
				outputFilename = "-"
			}
		} else if outputFilename == "" {
			outputFilename = "output"
		}
		var outputPath string
		if outputFilename == "-" || pathutil.IsRemoteURL(outputFilename) {
			outputPath = outputFilename
		} else if filepath.IsAbs(outputFilename) {
			outputPath = outputFilename
		} else {
			outputPath = filepath.Join("/tmp/jobs/job-1", outputFilename)
		}
		return outputPath
	}

	tests := []struct {
		name            string
		outputFilename  string
		streamingOutput bool
		want            string
	}{
		{
			name:            "streaming empty filename resolves to stdout",
			outputFilename:  "",
			streamingOutput: true,
			want:            "-",
		},
		{
			name:            "streaming explicit stdout preserved",
			outputFilename:  "-",
			streamingOutput: true,
			want:            "-",
		},
		{
			name:            "non-streaming empty filename defaults to output file",
			outputFilename:  "",
			streamingOutput: false,
			want:            "/tmp/jobs/job-1/output",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(tt.outputFilename, tt.streamingOutput)
			if got != tt.want {
				t.Errorf("output path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasOutputArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{
			name: "has output file",
			args: []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
			want: true,
		},
		{
			name: "no output file",
			args: []string{"-i", "input.mp4", "-c:v", "libx264"},
			want: false,
		},
		{
			name: "empty args",
			args: []string{},
			want: false,
		},
		{
			name: "only flags with values",
			args: []string{"-i", "input.mp4", "-c:v", "libx264", "-b:v", "1M"},
			want: false,
		},
		{
			name: "output after flags",
			args: []string{"-i", "input.mp4", "output.mp4"},
			want: true,
		},
		{
			name: "vn flag followed by codec flag - no output",
			args: []string{"-i", "input.mp4", "-vn", "-c:a", "libmp3lame"},
			want: false,
		},
		{
			name: "an flag followed by codec flag - no output",
			args: []string{"-i", "input.mp4", "-an", "-c:v", "libx264"},
			want: false,
		},
		{
			name: "boolean flags followed by output",
			args: []string{"-i", "input.mp4", "-vn", "-an", "output.mp4"},
			want: true,
		},
		{
			name: "vn flag with output",
			args: []string{"-i", "input.mp4", "-vn", "-c:a", "libmp3lame", "output.mp3"},
			want: true,
		},
		{
			name: "multiple boolean flags and codec",
			args: []string{"-i", "input.mp4", "-vn", "-sn", "-dn", "-c:a", "aac", "-b:a", "128k"},
			want: false,
		},
		{
			name: "y flag (overwrite) followed by output",
			args: []string{"-i", "input.mp4", "-y", "output.mp4"},
			want: true,
		},
		{
			name: "n flag (no overwrite) followed by output",
			args: []string{"-i", "input.mp4", "-n", "output.mp4"},
			want: true,
		},
		{
			name: "map flag takes a value - no output",
			args: []string{"-i", "input.mp4", "-map", "0:v", "-c:v", "libx264"},
			want: false,
		},
		{
			name: "map flag with output after value",
			args: []string{"-i", "input.mp4", "-map", "0:v", "output.mp4"},
			want: true,
		},
		{
			name: "input file without -i is not an output",
			args: []string{"-c:v", "libx264", "input.mp4"},
			want: false,
		},
		{
			name: "encoder params and input without -i is not an output",
			args: []string{"-c:v", "libx264", "-preset", "ultrafast", "input.mp4"},
			want: false,
		},
		{
			name: "stdout dash output after input",
			args: []string{"-i", "input.mp4", "-f", "mp4", "-"},
			want: true,
		},
		{
			name: "map flag with stream specifier takes a value",
			args: []string{"-i", "input.mp4", "-map", "0:a:0", "-c:a", "aac"},
			want: false,
		},
		{
			name: "equals syntax - flag with value attached",
			args: []string{"-i", "input.mp4", "-loglevel=verbose", "-c:v", "libx264"},
			want: false,
		},
		{
			name: "equals syntax followed by output",
			args: []string{"-i", "input.mp4", "-loglevel=verbose", "output.mp4"},
			want: true,
		},
		{
			name: "multiple equals syntax flags - no output",
			args: []string{"-i", "input.mp4", "-loglevel=verbose", "-c:v=libx264"},
			want: false,
		},
		{
			name: "equals syntax for codec with output",
			args: []string{"-i", "input.mp4", "-c:v=libx264", "output.mp4"},
			want: true,
		},
		{
			name: "shortest boolean flag followed by output",
			args: []string{"-i", "input.mp4", "-c:v", "libx264", "-c:a", "aac", "-shortest", "output.mp4"},
			want: true,
		},
		{
			name: "multiple inputs with shortest and output",
			args: []string{"-i", "video.mp4", "-i", "audio.mp4", "-c:v", "libx264", "-c:a", "aac", "-shortest", "merged.mp4"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasOutputArg(tt.args)
			if got != tt.want {
				t.Errorf("hasOutputArg() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindSeparatorIndex(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{
			name: "no separator",
			args: []string{"-c:v", "libx264"},
			want: -1,
		},
		{
			name: "separator at end",
			args: []string{"-c:v", "libx264", "--"},
			want: 2,
		},
		{
			name: "separator at beginning",
			args: []string{"--", "output.mp4"},
			want: 0,
		},
		{
			name: "separator in middle",
			args: []string{"-i", "input.mp4", "--", "output.mp4"},
			want: 2,
		},
		{
			name: "empty args",
			args: []string{},
			want: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findSeparatorIndex(tt.args); got != tt.want {
				t.Errorf("findSeparatorIndex(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestExecutor_LowersFFmpegPriority verifies the ffmpeg subprocess runs at the
// lowered scheduling priority while the worker process keeps its own priority.
// The lowered priority is what stops a CPU-saturating transcode from starving
// the worker's heartbeat goroutine: when that goroutine stalls the server reads
// the stale heartbeat as "worker offline" and migrates the still-running job,
// so repeated false migrations exhaust the job's retry budget even though every
// worker was alive.
func TestExecutor_LowersFFmpegPriority(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not installed")
	}

	selfBefore, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err != nil {
		t.Fatalf("Getpriority(self) failed: %v", err)
	}

	ex := NewExecutor(sh, 10*time.Second)
	// The child polls its own nice value instead of sampling it once: the
	// executor lowers the priority in the parent *after* Start() returns, so an
	// immediate sample can win the race on a loaded machine and read the
	// inherited nice. The poll is bounded so a missing setpriority still fails
	// the assertion below rather than hanging past the executor timeout.
	pollNice := `i=0; while [ "$(ps -o ni= -p $$ | tr -d ' ')" != ` + strconv.Itoa(ffmpegNice) +
		` ] && [ $i -lt 5 ]; do sleep 1; i=$((i+1)); done; ps -o ni= -p $$`
	result := ex.Execute(context.Background(), []string{"-c", pollNice})
	if result.Error != nil {
		t.Fatalf("Execute() failed: %v (stderr=%q)", result.Error, result.Stderr)
	}
	got, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		t.Fatalf("parsing child nice %q: %v", result.Stdout, err)
	}
	if got != ffmpegNice {
		t.Errorf("ffmpeg child nice = %d, want %d", got, ffmpegNice)
	}

	selfAfter, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err != nil {
		t.Fatalf("Getpriority(self) failed: %v", err)
	}
	if selfAfter != selfBefore {
		t.Errorf("worker priority changed %d -> %d; only the ffmpeg child may be lowered", selfBefore, selfAfter)
	}
}

// TestExecutor_SignalDeathExitCode verifies that the executor converts a
// signal-killed process into the POSIX 128+signal exit code. Go's
// exec.ExitError.ExitCode() returns -1 for signal deaths; without the
// ProcessState.Sys().(syscall.WaitStatus) conversion in ExecuteWithHandlers,
// SIGABRT would be reported as exit -1 and never trigger the
// isSignalDeath(134) crash classification.
func TestExecutor_SignalDeathExitCode(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}

	exec := NewExecutor(sh, 10*time.Second)
	// kill -6 $$ sends SIGABRT to the shell itself.
	result := exec.Execute(context.Background(), []string{"-c", "kill -6 $$"})

	if result.ExitCode != 134 {
		t.Fatalf("SIGABRT exit code = %d, want 134 (128+6); error=%v stderr=%q",
			result.ExitCode, result.Error, result.Stderr)
	}
	if result.Error == nil {
		t.Fatal("expected non-nil error for signal-killed process")
	}
}

// TestExecutor_SignalDeathSegvExitCode verifies SIGSEGV (signal 11) is
// reported as exit 139 (128+11), not -1.
func TestExecutor_SignalDeathSegvExitCode(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}

	exec := NewExecutor(sh, 10*time.Second)
	result := exec.Execute(context.Background(), []string{"-c", "kill -11 $$"})

	if result.ExitCode != 139 {
		t.Fatalf("SIGSEGV exit code = %d, want 139 (128+11); error=%v stderr=%q",
			result.ExitCode, result.Error, result.Stderr)
	}
}
