package worker

import (
	"testing"
)

func TestDefaultSoftwareFallbackChain(t *testing.T) {
	chain := defaultSoftwareFallbackChain()

	// Verify well-known chain entries exist
	tests := []struct {
		from     string
		expected string
	}{
		{"libx265", "libx264"},
		{"libvpx-vp9", "libx264"},
		{"libaom-av1", "libsvtav1"},
		{"libsvtav1", "libx264"},
		{"libx264rgb", "libx264"},
	}

	for _, tt := range tests {
		t.Run(tt.from, func(t *testing.T) {
			if got := chain[tt.from]; got != tt.expected {
				t.Errorf("defaultSoftwareFallbackChain()[%q] = %q, want %q", tt.from, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_GetAlternativeSoftwareEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected string
	}{
		{"libx265 -> libx264", "libx265", "libx264"},
		{"libvpx-vp9 -> libx264", "libvpx-vp9", "libx264"},
		{"libaom-av1 -> libsvtav1", "libaom-av1", "libsvtav1"},
		{"libsvtav1 -> libx264", "libsvtav1", "libx264"},
		{"libx264rgb -> libx264", "libx264rgb", "libx264"},
		{"no fallback for libx264", "libx264", ""},
		{"no fallback for unknown", "unknown_encoder", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.GetAlternativeSoftwareEncoder(tt.encoder); got != tt.expected {
				t.Errorf("GetAlternativeSoftwareEncoder(%q) = %q, want %q", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_AddSoftwareFallbackChain(t *testing.T) {
	fallback := NewEncoderFallback()

	// Add a custom chain entry
	fallback.AddSoftwareFallbackChain("custom_encoder", "fallback_encoder")

	if got := fallback.GetAlternativeSoftwareEncoder("custom_encoder"); got != "fallback_encoder" {
		t.Errorf("GetAlternativeSoftwareEncoder after AddSoftwareFallbackChain = %q, want %q", got, "fallback_encoder")
	}

	// Overwrite an existing chain entry
	fallback.AddSoftwareFallbackChain("libx265", "custom_fallback")

	if got := fallback.GetAlternativeSoftwareEncoder("libx265"); got != "custom_fallback" {
		t.Errorf("overwritten chain entry = %q, want %q", got, "custom_fallback")
	}
}

func TestEncoderFallback_SetUserSelectedEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	// Initially no user-selected encoder
	if fallback.IsUserSelectedEncoder("libx264") {
		t.Error("IsUserSelectedEncoder should return false when no encoder is set")
	}
	if fallback.IsUserSelectedEncoder("") {
		t.Error("IsUserSelectedEncoder should return false for empty encoder")
	}

	// Set the user-selected encoder
	fallback.SetUserSelectedEncoder("h264_nvenc")

	if !fallback.IsUserSelectedEncoder("h264_nvenc") {
		t.Error("IsUserSelectedEncoder should return true for the set encoder")
	}
	if fallback.IsUserSelectedEncoder("libx264") {
		t.Error("IsUserSelectedEncoder should return false for a different encoder")
	}

	// Change the user-selected encoder
	fallback.SetUserSelectedEncoder("hevc_qsv")

	if fallback.IsUserSelectedEncoder("h264_nvenc") {
		t.Error("IsUserSelectedEncoder should return false for previously set encoder after overwrite")
	}
	if !fallback.IsUserSelectedEncoder("hevc_qsv") {
		t.Error("IsUserSelectedEncoder should return true for newly set encoder")
	}

	// Set to empty string
	fallback.SetUserSelectedEncoder("")
	if fallback.IsUserSelectedEncoder("hevc_qsv") {
		t.Error("IsUserSelectedEncoder should return false after setting to empty string")
	}
}

func TestEncoderFallback_PrepareFallbackArgsWithSource_UserSelected(t *testing.T) {
	fallback := NewEncoderFallback()

	// User-selected hw encoder: should fallback to software equivalent
	args := []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "-preset", "fast", "output.mp4"}
	got := fallback.PrepareFallbackArgsWithSource(args, "output.mp4", true)
	if got == nil {
		t.Fatal("PrepareFallbackArgsWithSource returned nil for user-selected hw encoder")
	}

	encoder := extractEncoderFromArgs(got)
	if encoder != "libx264" {
		t.Errorf("encoder = %q, want %q", encoder, "libx264")
	}

	// User-selected software encoder: should return nil (no fallback needed)
	args2 := []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"}
	got2 := fallback.PrepareFallbackArgsWithSource(args2, "output.mp4", true)
	if got2 != nil {
		t.Errorf("PrepareFallbackArgsWithSource should return nil for user-selected software encoder, got %v", got2)
	}
}

func TestEncoderFallback_PrepareFallbackArgsWithSource_SystemFallback(t *testing.T) {
	fallback := NewEncoderFallback()

	// System-selected fallback encoder (e.g., libx265) — should trigger chained fallback
	args := []string{"-i", "input.mp4", "-c:v", "libx265", "output.mp4"}
	got := fallback.PrepareFallbackArgsWithSource(args, "output.mp4", false)
	if got == nil {
		t.Fatal("PrepareFallbackArgsWithSource returned nil for system-fallback encoder — expected chained fallback")
	}

	encoder := extractEncoderFromArgs(got)
	if encoder != "libx264" {
		t.Errorf("encoder = %q, want %q (chained from libx265)", encoder, "libx264")
	}

	// System-selected libx264 (end of chain): should return nil (no further fallback)
	args2 := []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"}
	got2 := fallback.PrepareFallbackArgsWithSource(args2, "output.mp4", false)
	if got2 != nil {
		t.Errorf("PrepareFallbackArgsWithSource should return nil for end-of-chain, got %v", got2)
	}
}

func TestEncoderFallback_PrepareFallbackArgs_WithUserSelectedEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	// Register the user's selected encoder
	fallback.SetUserSelectedEncoder("h264_nvenc")

	// PrepareFallbackArgs should detect this is user-selected and NOT chain
	args := []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"}
	got := fallback.PrepareFallbackArgs(args, "output.mp4")
	if got == nil {
		t.Fatal("PrepareFallbackArgs returned nil for user-selected hw encoder")
	}

	encoder := extractEncoderFromArgs(got)
	if encoder != "libx264" {
		t.Errorf("encoder = %q, want %q", encoder, "libx264")
	}
}

func TestEncoderFallback_buildFallbackArgs_OutputPathPreservation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		swEncoder  string
		outputPath string
		wantOutput bool // whether output path should appear in result
	}{
		{
			name:       "output already in args",
			args:       []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
			swEncoder:  "libx264",
			outputPath: "output.mp4",
			wantOutput: true,
		},
		{
			name:       "output missing from args",
			args:       []string{"-i", "input.mp4", "-c:v", "h264_nvenc"},
			swEncoder:  "libx264",
			outputPath: "output.mp4",
			wantOutput: true,
		},
		{
			name:       "empty outputPath should not append",
			args:       []string{"-i", "input.mp4", "-c:v", "h264_nvenc"},
			swEncoder:  "libx264",
			outputPath: "",
			wantOutput: false,
		},
		{
			name:       "hardware params stripped, output preserved",
			args:       []string{"-i", "input.mp4", "-c:v", "hevc_qsv", "-qsv_device", "/dev/dri/renderD128", "output.mp4"},
			swEncoder:  "libx265",
			outputPath: "output.mp4",
			wantOutput: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fallback := NewEncoderFallback()
			got := fallback.buildFallbackArgs(tt.args, tt.swEncoder, tt.outputPath)

			hasOutput := false
			for _, arg := range got {
				if arg == tt.outputPath {
					hasOutput = true
					break
				}
			}

			if tt.wantOutput && !hasOutput {
				t.Errorf("output path %q not found in args: %v", tt.outputPath, got)
			}
			if !tt.wantOutput && hasOutput {
				t.Errorf("output path should not be in args but found: %v", got)
			}

			// Also verify the encoder was replaced
			encoder := extractEncoderFromArgs(got)
			if encoder != tt.swEncoder {
				t.Errorf("encoder = %q, want %q", encoder, tt.swEncoder)
			}
		})
	}
}

func TestEncoderFallback_SoftwareFallbackChain_MultiStep(t *testing.T) {
	fallback := NewEncoderFallback()

	// AV1 multi-step chain: libaom-av1 -> libsvtav1 -> libx264 -> (end)
	args := []string{"-i", "input.mp4", "-c:v", "libaom-av1", "output.mp4"}

	// Step 1: as system fallback, libaom-av1 should chain to libsvtav1
	got := fallback.PrepareFallbackArgsWithSource(args, "output.mp4", false)
	if got == nil {
		t.Fatal("step 1: PrepareFallbackArgsWithSource returned nil")
	}
	if enc := extractEncoderFromArgs(got); enc != "libsvtav1" {
		t.Errorf("step 1 encoder = %q, want libsvtav1", enc)
	}

	// Step 2: as system fallback, libsvtav1 should chain to libx264
	got2 := fallback.PrepareFallbackArgsWithSource(got, "output.mp4", false)
	if got2 == nil {
		t.Fatal("step 2: PrepareFallbackArgsWithSource returned nil")
	}
	if enc := extractEncoderFromArgs(got2); enc != "libx264" {
		t.Errorf("step 2 encoder = %q, want libx264", enc)
	}

	// Step 3: libx264 is the end of chain
	got3 := fallback.PrepareFallbackArgsWithSource(got2, "output.mp4", false)
	if got3 != nil {
		t.Errorf("step 3: should return nil at end of chain, got %v", got3)
	}
}
