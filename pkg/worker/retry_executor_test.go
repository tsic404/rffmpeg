package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRetryStage_String(t *testing.T) {
	tests := []struct {
		name     string
		stage    RetryStage
		expected string
	}{
		{"initial", RetryStageInitial, "initial"},
		{"hardware_pruned", RetryStageHardwarePruned, "hardware_pruned"},
		{"advanced_pruned", RetryStageAdvancedPruned, "advanced_pruned"},
		{"software_fallback", RetryStageSoftwareFallback, "software_fallback"},
		{"exhausted", RetryStageExhausted, "exhausted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stage.String(); got != tt.expected {
				t.Errorf("RetryStage.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRetryStage_Description(t *testing.T) {
	tests := []struct {
		name  string
		stage RetryStage
	}{
		{"initial", RetryStageInitial},
		{"hardware_pruned", RetryStageHardwarePruned},
		{"advanced_pruned", RetryStageAdvancedPruned},
		{"software_fallback", RetryStageSoftwareFallback},
		{"exhausted", RetryStageExhausted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stage.Description(); got == "" {
				t.Error("RetryStage.Description() returned empty string")
			}
		})
	}
}

func TestDefaultRetryConfig(t *testing.T) {
	config := DefaultRetryConfig()

	if config.MaxRetries != 3 {
		t.Errorf("DefaultRetryConfig().MaxRetries = %v, want 3", config.MaxRetries)
	}
	if config.InitialInterval != 1*time.Second {
		t.Errorf("DefaultRetryConfig().InitialInterval = %v, want 1s", config.InitialInterval)
	}
	if config.UseExponentialBackoff {
		t.Error("DefaultRetryConfig().UseExponentialBackoff should be false")
	}
	if !config.EnableSoftwareFallback {
		t.Error("DefaultRetryConfig().EnableSoftwareFallback should be true")
	}
}

func TestExtractEncoderFromArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected string
	}{
		{
			name:     "c:v with separate value",
			args:     []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
			expected: "h264_nvenc",
		},
		{
			name:     "c:v with equals value is not a selection",
			args:     []string{"-i", "input.mp4", "-c:v=libx264", "output.mp4"},
			expected: "",
		},
		{
			name:     "vcodec with separate value",
			args:     []string{"-i", "input.mp4", "-vcodec", "hevc_qsv", "output.mp4"},
			expected: "hevc_qsv",
		},
		{
			name:     "vcodec with equals value is not a selection",
			args:     []string{"-i", "input.mp4", "-vcodec=libx265", "output.mp4"},
			expected: "",
		},
		{
			name:     "no encoder",
			args:     []string{"-i", "input.mp4", "output.mp4"},
			expected: "",
		},
		{
			name:     "codec:v with separate value",
			args:     []string{"-i", "input.mp4", "-codec:v", "vp9_vaapi", "output.mp4"},
			expected: "vp9_vaapi",
		},
		{
			name:     "codec:v with equals value is not a selection",
			args:     []string{"-i", "input.mp4", "-codec:v=h264_nvenc", "output.mp4"},
			expected: "",
		},
		{
			name:     "stream specifier -c:v:0",
			args:     []string{"-i", "input.mp4", "-c:v:0", "h264_vaapi", "output.mp4"},
			expected: "h264_vaapi",
		},
		{
			name:     "stream specifier -codec:v:0",
			args:     []string{"-i", "input.mp4", "-codec:v:0", "h264_vaapi", "output.mp4"},
			expected: "h264_vaapi",
		},
		{
			name:     "general -c with video encoder",
			args:     []string{"-i", "input.mp4", "-c", "h264_vaapi", "output.mp4"},
			expected: "h264_vaapi",
		},
		{
			name:     "duplicate declarations use the last one",
			args:     []string{"-i", "input.mp4", "-c:v", "libvpx-vp9", "-c:v", "vp9_qsv", "output.webm"},
			expected: "vp9_qsv",
		},
		{
			name:     "-vn after -- is not an encoder",
			args:     []string{"-i", "input.mp4", "--", "-c:v", "h264_vaapi", "output.mp4"},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractEncoderFromArgs(tt.args); got != tt.expected {
				t.Errorf("extractEncoderFromArgs() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRetryResult_GetAuditTrailSummary(t *testing.T) {
	result := &RetryResult{
		TotalAttempts:       3,
		FinalStage:          RetryStageAdvancedPruned,
		Success:             true,
		UsedSoftwareEncoder: false,
		OriginalEncoder:     "h264_nvenc",
		FinalEncoder:        "h264_nvenc",
		AuditTrail: []RetryAuditEntry{
			{
				Timestamp:     time.Now(),
				Stage:         RetryStageInitial,
				AttemptNumber: 1,
				ExitCode:      1,
				Success:       false,
				ErrorType:     "hwaccel_failed",
			},
			{
				Timestamp:     time.Now(),
				Stage:         RetryStageHardwarePruned,
				AttemptNumber: 2,
				ExitCode:      0,
				Success:       true,
			},
		},
	}

	summary := result.GetAuditTrailSummary()

	if summary == "" {
		t.Error("GetAuditTrailSummary() returned empty string")
	}
}

// MockExecutor is a mock executor for testing.
type MockExecutor struct {
	results []ExecResult
	callIdx int
}

func (m *MockExecutor) Execute(ctx interface{}, args []string) ExecResult {
	if m.callIdx < len(m.results) {
		result := m.results[m.callIdx]
		m.callIdx++
		return result
	}
	return ExecResult{ExitCode: 0}
}

func TestEncoderFallback_IsHardwareEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"nvenc", "h264_nvenc", true},
		{"qsv", "hevc_qsv", true},
		{"vaapi", "h264_vaapi", true},
		{"amf", "hevc_amf", true},
		{"videotoolbox", "h264_videotoolbox", true},
		{"software x264", "libx264", false},
		{"software x265", "libx265", false},
		{"software vp9", "libvpx-vp9", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.IsHardwareEncoder(tt.encoder); got != tt.expected {
				t.Errorf("IsHardwareEncoder(%v) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_GetSoftwareEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected string
	}{
		{"h264_nvenc", "h264_nvenc", "libx264"},
		{"hevc_nvenc", "hevc_nvenc", "libx265"},
		{"h264_qsv", "h264_qsv", "libx264"},
		{"hevc_qsv", "hevc_qsv", "libx265"},
		{"h264_vaapi", "h264_vaapi", "libx264"},
		{"vp9_vaapi", "vp9_vaapi", "libvpx-vp9"},
		{"libx264", "libx264", "libx264"},
		{"libx265", "libx265", "libx265"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.GetSoftwareEncoder(tt.encoder); got != tt.expected {
				t.Errorf("GetSoftwareEncoder(%v) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_PrepareFallbackArgs(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name        string
		args        []string
		outputPath  string
		wantNil     bool
		wantEncoder string
	}{
		{
			name:        "nvenc fallback",
			args:        []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "-preset", "fast", "output.mp4"},
			outputPath:  "output.mp4",
			wantNil:     false,
			wantEncoder: "libx264",
		},
		{
			name:       "already software encoder",
			args:       []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
			outputPath: "output.mp4",
			wantNil:    true, // No fallback needed for software encoder
		},
		{
			name:       "no encoder specified",
			args:       []string{"-i", "input.mp4", "output.mp4"},
			outputPath: "output.mp4",
			wantNil:    true, // Cannot determine fallback
		},
		{
			name:        "qsv fallback with hardware params",
			args:        []string{"-i", "input.mp4", "-c:v", "hevc_qsv", "-qsv_device", "/dev/dri/renderD128", "-preset", "fast", "output.mp4"},
			outputPath:  "output.mp4",
			wantNil:     false,
			wantEncoder: "libx265",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fallback.PrepareFallbackArgs(tt.args, tt.outputPath, true)

			if tt.wantNil {
				if got != nil {
					t.Errorf("PrepareFallbackArgs() = %v, want nil", got)
				}
				return
			}

			if got == nil {
				t.Error("PrepareFallbackArgs() returned nil, want non-nil")
				return
			}

			// Check that encoder was replaced
			encoder := extractEncoderFromArgs(got)
			if encoder != tt.wantEncoder {
				t.Errorf("PrepareFallbackArgs() encoder = %v, want %v", encoder, tt.wantEncoder)
			}
		})
	}
}

func TestEncoderFallback_GetEncoderFormat(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected string
	}{
		{"h264_nvenc", "h264_nvenc", "h264"},
		{"hevc_nvenc", "hevc_nvenc", "hevc"},
		{"libx264", "libx264", "h264"},
		{"libx265", "libx265", "hevc"},
		{"vp9_vaapi", "vp9_vaapi", "vp9"},
		{"libvpx-vp9", "libvpx-vp9", "vp9"},
		{"libvpx", "libvpx", "vp8"},
		{"av1_nvenc", "av1_nvenc", "av1"},
		{"libsvtav1", "libsvtav1", "av1"},
		{"svt_av1", "svt_av1", "av1"},
		{"svt-av1", "svt-av1", "av1"},
		{"svt-hevc", "svt-hevc", "hevc"},
		{"libopus", "libopus", "opus"},
		{"opus", "opus", "opus"},
		{"libmp3lame", "libmp3lame", "mp3"},
		{"mp3", "mp3", "mp3"},
		{"fdk-aac", "fdk-aac", "aac"},
		{"aac", "aac", "aac"},
		{"libvorbis", "libvorbis", "vorbis"},
		{"flac", "flac", "flac"},
		{"pcm_s16le", "pcm_s16le", "pcm"},
		{"wav", "wav", "wav"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.GetEncoderFormat(tt.encoder); got != tt.expected {
				t.Errorf("GetEncoderFormat(%v) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_GetAvailableSoftwareEncoders(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name          string
		format        string
		expectCount   int
		expectEncoder string
	}{
		{"h264", "h264", 2, "libx264"},
		{"hevc", "hevc", 1, "libx265"},
		{"vp9", "vp9", 1, "libvpx-vp9"},
		{"unknown", "unknown", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fallback.GetAvailableSoftwareEncoders(tt.format)

			if len(got) != tt.expectCount {
				t.Errorf("GetAvailableSoftwareEncoders(%v) count = %v, want %v", tt.format, len(got), tt.expectCount)
			}

			if tt.expectEncoder != "" && len(got) > 0 {
				found := false
				for _, e := range got {
					if e == tt.expectEncoder {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("GetAvailableSoftwareEncoders(%v) missing expected encoder %v", tt.format, tt.expectEncoder)
				}
			}
		})
	}
}

func TestEncoderFallback_AddEncoderMapping(t *testing.T) {
	fallback := NewEncoderFallback()

	// Add a new mapping
	fallback.AddEncoderMapping("custom_hw_encoder", "custom_sw_encoder")

	// Verify it was added
	got := fallback.GetSoftwareEncoder("custom_hw_encoder")
	if got != "custom_sw_encoder" {
		t.Errorf("GetSoftwareEncoder() = %v, want custom_sw_encoder", got)
	}
}

func TestEncoderFallback_ValidateEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"h264_nvenc", "h264_nvenc", true},
		{"libx264", "libx264", true},
		{"unknown_encoder", "unknown_encoder", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.ValidateEncoder(tt.encoder); got != tt.expected {
				t.Errorf("ValidateEncoder(%v) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_isHardwareSpecificParam(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		param    string
		expected bool
	}{
		{"-hwaccel", true},
		{"-hwaccel_device", true},
		{"-vaapi_device", true},
		{"-qsv_device", true},
		{"-gpu", true},
		{"-i", false},
		{"-c:v", false},
		{"-preset", false},
	}

	for _, tt := range tests {
		t.Run(tt.param, func(t *testing.T) {
			if got := fallback.isHardwareSpecificParam(tt.param); got != tt.expected {
				t.Errorf("isHardwareSpecificParam(%v) = %v, want %v", tt.param, got, tt.expected)
			}
		})
	}
}

func TestRetryAuditEntry(t *testing.T) {
	entry := RetryAuditEntry{
		Timestamp:     time.Now(),
		Stage:         RetryStageInitial,
		AttemptNumber: 1,
		Args:          []string{"-i", "input.mp4", "output.mp4"},
		ExitCode:      0,
		Success:       true,
	}

	if entry.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
	if entry.Stage != RetryStageInitial {
		t.Errorf("Stage = %v, want %v", entry.Stage, RetryStageInitial)
	}
}

func TestRetryResult(t *testing.T) {
	result := &RetryResult{
		FinalResult: ExecResult{
			ExitCode: 0,
			Stdout:   "",
			Stderr:   "",
		},
		TotalAttempts:       1,
		FinalStage:          RetryStageInitial,
		Success:             true,
		UsedSoftwareEncoder: false,
		OriginalEncoder:     "h264_nvenc",
		FinalEncoder:        "h264_nvenc",
		AuditTrail:          []RetryAuditEntry{},
	}

	if !result.Success {
		t.Error("Success should be true")
	}
	if result.TotalAttempts != 1 {
		t.Errorf("TotalAttempts = %v, want 1", result.TotalAttempts)
	}
}

func TestPruneHardwareParams_SwitchesEncoder(t *testing.T) {
	// Create a RetryExecutor with a mock executor and real fallback/pruner
	fallback := NewEncoderFallback()
	pruner := NewParamPruner()
	executor := NewExecutor("ffmpeg", 2*time.Hour)

	re := &RetryExecutor{
		executor:    executor,
		interceptor: NewErrorInterceptor(),
		pruner:      pruner,
		config:      DefaultRetryConfig(),
		fallback:    fallback,
	}

	// Input args with h264_qsv hardware encoder + hardware device params
	args := []string{"-i", "input.mp4", "-c:v", "h264_qsv", "-qsv_device", "/dev/dri/renderD128", "-preset", "fast", "output.mp4"}

	prunedArgs, prunedParams := re.pruneHardwareParams(args)

	// Verify encoder was switched to software
	encoder := extractEncoderFromArgs(prunedArgs)
	if encoder != "libx264" {
		t.Errorf("Expected encoder libx264, got %s", encoder)
	}

	// Verify hardware params were removed
	for _, arg := range prunedArgs {
		if arg == "h264_qsv" {
			t.Error("h264_qsv encoder should have been replaced")
		}
		if arg == "-qsv_device" || arg == "/dev/dri/renderD128" {
			t.Errorf("Hardware param %s should have been pruned", arg)
		}
	}

	// Verify pruned params list includes encoder switch
	foundEncoderSwitch := false
	for _, p := range prunedParams {
		if p == "encoder:h264_qsv->libx264" {
			foundEncoderSwitch = true
		}
	}
	if !foundEncoderSwitch {
		t.Errorf("Expected encoder switch in pruned params, got %v", prunedParams)
	}
}

func TestPruneHardwareParams_NoSwitchForSoftwareEncoder(t *testing.T) {
	fallback := NewEncoderFallback()
	pruner := NewParamPruner()
	executor := NewExecutor("ffmpeg", 2*time.Hour)

	re := &RetryExecutor{
		executor:    executor,
		interceptor: NewErrorInterceptor(),
		pruner:      pruner,
		config:      DefaultRetryConfig(),
		fallback:    fallback,
	}

	// Input args with software encoder (libx264) + no hardware params
	args := []string{"-i", "input.mp4", "-c:v", "libx264", "-preset", "fast", "output.mp4"}

	prunedArgs, _ := re.pruneHardwareParams(args)

	// Verify encoder was NOT switched (already software)
	encoder := extractEncoderFromArgs(prunedArgs)
	if encoder != "libx264" {
		t.Errorf("Expected encoder libx264 to remain, got %s", encoder)
	}
}

// TestExecuteWithRetry_StdoutOutputSkipsFileValidation verifies that when the
// output path is "-" (ffmpeg stdout, streaming mode), ExecuteWithRetry skips
// file existence/size validation on success — otherwise os.Stat("-") fails and
// a successful streaming job is misclassified as output_empty.
func TestExecuteWithRetry_StdoutOutputSkipsFileValidation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	executor := NewExecutor("ffmpeg", time.Minute)
	re := &RetryExecutor{
		executor:    executor,
		interceptor: NewErrorInterceptor(),
		pruner:      NewParamPruner(),
		config:      DefaultRetryConfig(),
		fallback:    NewEncoderFallback(),
	}

	// ffmpeg exits 0 writing only to stdout ("-"); no local file is created.
	args := []string{"-f", "lavfi", "-i", "testsrc=duration=0.1", "-f", "mpegts", "-"}
	result := re.ExecuteWithRetry(context.Background(), args, "-", false, nil, nil, nil)

	if !result.Success {
		t.Fatalf("expected success for stdout output, got stage=%s attempts=%d",
			result.FinalStage, result.TotalAttempts)
	}
	for _, entry := range result.AuditTrail {
		if entry.ErrorType == ErrorTypeOutputEmpty.String() {
			t.Fatalf("stdout output misclassified as %s: %s", entry.ErrorType, entry.ErrorMessage)
		}
	}
}

// signalCrashExecutor returns a SIGABRT (exit 134) crash on the first call,
// then success — simulating ffmpeg's sporadic self-abort under load.
type signalCrashExecutor struct {
	called int
}

func (s *signalCrashExecutor) Execute(ctx context.Context, args []string) ExecResult {
	s.called++
	if s.called == 1 {
		return ExecResult{
			ExitCode: 134,
			Stderr:   "frame=  120 fps= 30 q=28.0\nAborted\n",
			Error:    fmt.Errorf("ffmpeg exited with code 134"),
		}
	}
	return ExecResult{ExitCode: 0, Stdout: "ok"}
}

func (s *signalCrashExecutor) ExecuteWithHandlers(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult {
	return s.Execute(ctx, args)
}

// TestExecuteWithRetry_SigAbortRetriesAndSucceeds locks the fix:
// a SIGABRT (exit 134) is classified as a retryable process crash, so the
// retry executor re-runs the job and succeeds on the second attempt instead
// of permanently failing.
func TestExecuteWithRetry_SigAbortRetriesAndSucceeds(t *testing.T) {
	exec := &signalCrashExecutor{}
	re := &RetryExecutor{
		executor:    exec,
		interceptor: NewErrorInterceptor(),
		pruner:      NewParamPruner(),
		config: &RetryConfig{
			MaxRetries:             3,
			InitialInterval:        1 * time.Millisecond,
			EnableSoftwareFallback: false,
		},
		fallback: NewEncoderFallback(),
	}

	args := []string{"-i", "input.mp4", "-c:v", "libx264", "-f", "mpegts", "pipe:1"}
	result := re.ExecuteWithRetry(context.Background(), args, "-", false, nil, nil, nil)

	if !result.Success {
		t.Fatalf("expected retry success after SIGABRT, got stage=%s attempts=%d",
			result.FinalStage, result.TotalAttempts)
	}
	if result.TotalAttempts < 2 {
		t.Fatalf("expected at least 2 attempts (crash + retry), got %d", result.TotalAttempts)
	}
	if exec.called < 2 {
		t.Fatalf("executor called %d times, expected ≥ 2", exec.called)
	}
	// Verify the crash was classified as a process crash, not a generic error.
	if len(result.AuditTrail) == 0 {
		t.Fatal("expected audit trail entries")
	}
	first := result.AuditTrail[0]
	if first.ErrorType != ErrorTypeProcessCrash.String() {
		t.Errorf("first attempt error type = %s, want %s", first.ErrorType, ErrorTypeProcessCrash.String())
	}
}

// recordingExecutor records, per call, whether a non-nil stderrHandler reached
// ExecuteWithHandlers and the stderr chunk the handler received. It is used to
// lock the contract that the retry executor streams stderr on every
// attempt, not just the first.
type recordingExecutor struct {
	calls   []recordingExecutorCall
	failure bool
}

type recordingExecutorCall struct {
	handlerSet     bool
	streamedStderr string
}

func (r *recordingExecutor) Execute(ctx context.Context, args []string) ExecResult {
	return r.run(ctx, args, nil, nil)
}

func (r *recordingExecutor) ExecuteWithHandlers(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult {
	return r.run(ctx, args, stdoutHandler, stderrHandler)
}

func (r *recordingExecutor) run(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult {
	call := recordingExecutorCall{}
	if stderrHandler != nil {
		call.handlerSet = true
		stderr := "frame=  10 fps= 30 q=28.0\nAborted\n"
		stderrHandler(stderr)
		call.streamedStderr = stderr
	}
	r.calls = append(r.calls, call)

	if r.failure && len(r.calls) == 1 {
		return ExecResult{
			ExitCode: 134,
			Stderr:   "frame=  10 fps= 30 q=28.0\nAborted\n",
			Error:    fmt.Errorf("ffmpeg exited with code 134"),
		}
	}
	return ExecResult{ExitCode: 0, Stdout: "ok"}
}

// TestExecuteWithRetry_StreamsStderrEveryAttempt locks the
// fix: when a stderrHandler is supplied, it must reach the executor on every
// retry attempt so the full ffmpeg log streams live each time — the
// deduplication contract (concise terminal Error + per-attempt streaming)
// depends on it.
func TestExecuteWithRetry_StreamsStderrEveryAttempt(t *testing.T) {
	exec := &recordingExecutor{failure: true}
	re := &RetryExecutor{
		executor:    exec,
		interceptor: NewErrorInterceptor(),
		pruner:      NewParamPruner(),
		config: &RetryConfig{
			MaxRetries:             2,
			InitialInterval:        1 * time.Millisecond,
			EnableSoftwareFallback: false,
		},
		fallback: NewEncoderFallback(),
	}

	var streamed []string
	handler := func(chunk string) { streamed = append(streamed, chunk) }

	result := re.ExecuteWithRetry(context.Background(), []string{"-f", "lavfi", "-i", "testsrc", "-f", "mpegts", "-"}, "-", false, nil, handler, nil)

	if !result.Success {
		t.Fatalf("expected retry success after SIGABRT, got stage=%s attempts=%d",
			result.FinalStage, result.TotalAttempts)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(exec.calls))
	}
	for i, call := range exec.calls {
		if !call.handlerSet {
			t.Errorf("attempt %d: stderrHandler was nil", i+1)
		}
	}
	if len(streamed) != 2 {
		t.Fatalf("stderr handler called %d times, want 2", len(streamed))
	}
	for i, chunk := range streamed {
		if chunk == "" {
			t.Errorf("attempt %d: streamed empty stderr", i+1)
		}
	}
}

// spellingFallbackExecutor fails while the args select a hardware encoder and
// succeeds once the retry path has switched to software.
type spellingFallbackExecutor struct{}

func (s *spellingFallbackExecutor) Execute(ctx context.Context, args []string) ExecResult {
	return s.ExecuteWithHandlers(ctx, args, nil, nil)
}

func (s *spellingFallbackExecutor) ExecuteWithHandlers(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult {
	encoder := extractEncoderFromArgs(args)
	if encoder != "" && NewEncoderFallback().IsHardwareEncoder(encoder) {
		return ExecResult{ExitCode: 1, Stderr: "[error] Invalid argument\n"}
	}
	return ExecResult{ExitCode: 0}
}

// TestExecuteWithRetry_FallbackCoversEncoderSpellings is the end-to-end retry
// regression: every recognized video-encoder spelling must resolve far enough
// for the hardware_pruned stage to switch to the same-family software encoder.
func TestExecuteWithRetry_FallbackCoversEncoderSpellings(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantFinal string
	}{
		{
			name:      "stream specifier -c:v:0",
			args:      []string{"-i", "in.mp4", "-c:v:0", "h264_vaapi", "out.mp4"},
			wantFinal: "libx264",
		},
		{
			name:      "stream specifier -codec:v:0",
			args:      []string{"-i", "in.mp4", "-codec:v:0", "h264_vaapi", "out.mp4"},
			wantFinal: "libx264",
		},
		{
			name:      "general -c",
			args:      []string{"-i", "in.mp4", "-c", "h264_vaapi", "out.mp4"},
			wantFinal: "libx264",
		},
		{
			name:      "duplicate declarations use the last one",
			args:      []string{"-i", "in.mkv", "-c:v", "libvpx-vp9", "-c:v", "vp9_qsv", "out.webm"},
			wantFinal: "libvpx-vp9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re := &RetryExecutor{
				executor:    &spellingFallbackExecutor{},
				interceptor: NewErrorInterceptor(),
				pruner:      NewParamPruner(),
				config: &RetryConfig{
					MaxRetries:             3,
					InitialInterval:        0,
					EnableSoftwareFallback: true,
				},
				fallback: NewEncoderFallback(),
			}

			result := re.ExecuteWithRetry(context.Background(), tt.args, "-", false, nil, nil, nil)
			if !result.Success {
				t.Fatalf("retry did not recover: stage=%s final=%s attempts=%d",
					result.FinalStage, result.FinalEncoder, result.TotalAttempts)
			}
			if result.FinalEncoder != tt.wantFinal {
				t.Errorf("final encoder = %q, want %q", result.FinalEncoder, tt.wantFinal)
			}
			if result.FinalStage != RetryStageHardwarePruned {
				t.Errorf("final stage = %s, want %s", result.FinalStage, RetryStageHardwarePruned)
			}
		})
	}
}

// copyFailExecutor fails every attempt the way ffmpeg rejects an invalid
// argument; the retry path must not turn it into a successful re-encode.
type copyFailExecutor struct{}

func (c *copyFailExecutor) Execute(ctx context.Context, args []string) ExecResult {
	return c.ExecuteWithHandlers(ctx, args, nil, nil)
}

func (c *copyFailExecutor) ExecuteWithHandlers(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult {
	return ExecResult{ExitCode: 1, Stderr: "[error] Invalid argument\n"}
}

// TestExecuteWithRetry_CopyNeverRewritten is the blocking regression: copy is a
// stream-copy directive, not an unspecified encoder, so retry/fallback must
// leave argv unchanged and never claim a software fallback.
func TestExecuteWithRetry_CopyNeverRewritten(t *testing.T) {
	forms := [][]string{
		{"-i", "in.mp4", "-c", "copy", "out.mp4"},
		{"-i", "in.mp4", "-c:v", "copy", "out.mp4"},
		{"-i", "in.mp4", "-c:v:0", "copy", "out.mkv"},
	}
	for _, args := range forms {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			re := &RetryExecutor{
				executor:    &copyFailExecutor{},
				interceptor: NewErrorInterceptor(),
				pruner:      NewParamPruner(),
				config: &RetryConfig{
					MaxRetries:             3,
					InitialInterval:        0,
					EnableSoftwareFallback: true,
				},
				fallback: NewEncoderFallback(),
			}

			result := re.ExecuteWithRetry(context.Background(), args, "-", false, nil, nil, nil)
			if result.Success {
				t.Fatalf("copy rewrite must not report success")
			}
			if result.UsedSoftwareEncoder {
				t.Errorf("copy must not be reported as a software fallback")
			}
			if result.FinalEncoder != "copy" {
				t.Errorf("final encoder = %q, want copy", result.FinalEncoder)
			}
			if len(result.AuditTrail) == 0 {
				t.Fatal("expected retry attempts")
			}
			for _, entry := range result.AuditTrail {
				if !slices.Equal(entry.Args, args) {
					t.Errorf("copy argv changed at attempt %d: got %v, want %v", entry.AttemptNumber, entry.Args, args)
				}
			}
		})
	}
}

// finalFallbackStaleOutputExecutor emulates the two ffmpeg behaviours this
// path meets: a hardware attempt creates the output file and then fails to
// open its device, and a software attempt runs under native no -y semantics,
// which refuses to overwrite an existing output.
type finalFallbackStaleOutputExecutor struct {
	outputPath   string
	staleSeen    bool
	fallbackCall int
}

func (f *finalFallbackStaleOutputExecutor) Execute(ctx context.Context, args []string) ExecResult {
	return f.ExecuteWithHandlers(ctx, args, nil, nil)
}

func (f *finalFallbackStaleOutputExecutor) ExecuteWithHandlers(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult {
	if encoder := extractEncoderFromArgsWithCaps(args, nil); encoder != "libx264" {
		if err := os.WriteFile(f.outputPath, nil, 0o644); err != nil {
			return ExecResult{ExitCode: 1, Error: err}
		}
		return ExecResult{
			ExitCode: 1,
			Stderr:   "Cannot open device /dev/video0: No such file or directory\n",
			Error:    fmt.Errorf("ffmpeg exited with code 1"),
		}
	}

	f.fallbackCall++
	if _, err := os.Stat(f.outputPath); err == nil {
		f.staleSeen = true
		return ExecResult{
			ExitCode: 1,
			Stderr:   "File '" + f.outputPath + "' already exists. Overwrite? [y/N] Not overwriting - exiting\n",
			Error:    fmt.Errorf("ffmpeg exited with code 1"),
		}
	}
	if err := os.WriteFile(f.outputPath, []byte("transcoded"), 0o644); err != nil {
		return ExecResult{ExitCode: 1, Error: err}
	}
	return ExecResult{ExitCode: 0}
}

// TestExecuteWithRetry_FinalFallbackClearsStaleOutput locks the fix: the final
// software fallback is a fresh ffmpeg process and must clear the output the
// previous failed attempt left behind, exactly like a retry attempt. Without
// it, ffmpeg's no -y refusal turns an available same-family software encoder
// into a hard failure.
func TestExecuteWithRetry_FinalFallbackClearsStaleOutput(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "out.mp4")
	exec := &finalFallbackStaleOutputExecutor{outputPath: outputPath}
	re := &RetryExecutor{
		executor:    exec,
		interceptor: NewErrorInterceptor(),
		pruner:      NewParamPruner(),
		config: &RetryConfig{
			MaxRetries:             1,
			InitialInterval:        0,
			EnableSoftwareFallback: true,
		},
		fallback: NewEncoderFallback(),
	}

	args := []string{"-i", "in.mp4", "-c:v", "h264_v4l2m2m", outputPath}
	result := re.ExecuteWithRetry(context.Background(), args, outputPath, false, nil, nil, nil)

	if exec.fallbackCall != 1 {
		t.Fatalf("final software fallback attempted %d times, want 1", exec.fallbackCall)
	}
	if exec.staleSeen {
		t.Fatal("final software fallback started with the stale output still present")
	}
	if !result.Success {
		t.Fatalf("expected final software fallback to succeed: stage=%s attempts=%d",
			result.FinalStage, result.TotalAttempts)
	}
	if result.FinalStage != RetryStageSoftwareFallback || result.FinalEncoder != "libx264" {
		t.Errorf("final stage/encoder = %s/%s, want %s/libx264",
			result.FinalStage, result.FinalEncoder, RetryStageSoftwareFallback)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("stat fallback output: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("fallback output is empty")
	}
}
