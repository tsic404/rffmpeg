package worker

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestDefaultSoftwareFallbackChain(t *testing.T) {
	chain := defaultSoftwareFallbackChain()

	// Verify well-known same-family chain entries exist
	tests := []struct {
		from     string
		expected string
	}{
		{"libaom-av1", "libsvtav1"},
		{"libx264rgb", "libx264"},
	}

	for _, tt := range tests {
		t.Run(tt.from, func(t *testing.T) {
			if got := chain[tt.from]; got != tt.expected {
				t.Errorf("defaultSoftwareFallbackChain()[%q] = %q, want %q", tt.from, got, tt.expected)
			}
		})
	}

	// Cross-format transitions must not be configured (TSI-2671).
	for _, cross := range []string{"libx265", "libvpx-vp9", "libsvtav1"} {
		if got, ok := chain[cross]; ok {
			t.Errorf("defaultSoftwareFallbackChain()[%q] = %q, want absent (cross-format)", cross, got)
		}
	}
}

func TestEncoderFallback_GetAlternativeSoftwareEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected string
	}{
		{"libaom-av1 -> libsvtav1", "libaom-av1", "libsvtav1"},
		{"libx264rgb -> libx264", "libx264rgb", "libx264"},
		{"no fallback for libx265", "libx265", ""},
		{"no fallback for libvpx-vp9", "libvpx-vp9", ""},
		{"no fallback for libsvtav1", "libsvtav1", ""},
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

	// Same-family custom entry is accepted at read time.
	fallback.AddSoftwareFallbackChain("libx264", "libx264rgb")
	if got := fallback.GetAlternativeSoftwareEncoder("libx264"); got != "libx264rgb" {
		t.Errorf("GetAlternativeSoftwareEncoder after AddSoftwareFallbackChain = %q, want %q", got, "libx264rgb")
	}

	// Unknown-format entries are refused at read time (TSI-2685).
	fallback.AddSoftwareFallbackChain("custom_encoder", "fallback_encoder")
	if got := fallback.GetAlternativeSoftwareEncoder("custom_encoder"); got != "" {
		t.Errorf("unknown-format chain entry = %q, want empty (refused)", got)
	}

	// Overwriting an entry with a cross-format target is refused at read time.
	fallback.AddSoftwareFallbackChain("libx265", "libx264")
	if got := fallback.GetAlternativeSoftwareEncoder("libx265"); got != "" {
		t.Errorf("cross-format overwritten chain entry = %q, want empty (refused)", got)
	}
}

func TestEncoderFallback_PrepareFallbackArgs_IsUserSelected(t *testing.T) {
	fallback := NewEncoderFallback()

	// User-selected hw encoder: falls back to the software equivalent.
	args := []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"}
	got := fallback.PrepareFallbackArgs(args, "output.mp4", true)
	if got == nil {
		t.Fatal("PrepareFallbackArgs returned nil for user-selected hw encoder")
	}
	if encoder := extractEncoderFromArgs(got); encoder != "libx264" {
		t.Errorf("encoder = %q, want %q", encoder, "libx264")
	}

	// User-selected software encoder: no fallback needed → nil.
	args2 := []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"}
	if got2 := fallback.PrepareFallbackArgs(args2, "output.mp4", true); got2 != nil {
		t.Errorf("PrepareFallbackArgs should return nil for user-selected software encoder, got %v", got2)
	}

	// System-chosen libx265 (isUserSelected=false): cross-format chain is refused,
	// so no alternative is available (TSI-2671).
	args3 := []string{"-i", "input.mp4", "-c:v", "libx265", "output.mp4"}
	if got3 := fallback.PrepareFallbackArgs(args3, "output.mp4", false); got3 != nil {
		t.Errorf("PrepareFallbackArgs should return nil for cross-format fallback, got %v", got3)
	}
	// System-chosen libx264 (end of chain): nil.
	if got4 := fallback.PrepareFallbackArgs(args2, "output.mp4", false); got4 != nil {
		t.Errorf("PrepareFallbackArgs should return nil for end-of-chain, got %v", got4)
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

	// System-selected fallback encoder (e.g., libx265) — cross-format chain is
	// refused, so no alternative is available (TSI-2671).
	args := []string{"-i", "input.mp4", "-c:v", "libx265", "output.mp4"}
	if got := fallback.PrepareFallbackArgsWithSource(args, "output.mp4", false); got != nil {
		t.Errorf("PrepareFallbackArgsWithSource should return nil for cross-format fallback, got %v", got)
	}

	// System-selected libx264 (end of chain): should return nil (no further fallback)
	args2 := []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"}
	got2 := fallback.PrepareFallbackArgsWithSource(args2, "output.mp4", false)
	if got2 != nil {
		t.Errorf("PrepareFallbackArgsWithSource should return nil for end-of-chain, got %v", got2)
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

	// AV1 same-format chain: libaom-av1 -> libsvtav1 -> (end; cross-format refused)
	args := []string{"-i", "input.mp4", "-c:v", "libaom-av1", "output.mp4"}

	// Step 1: as system fallback, libaom-av1 should chain to libsvtav1 (same family)
	got := fallback.PrepareFallbackArgsWithSource(args, "output.mp4", false)
	if got == nil {
		t.Fatal("step 1: PrepareFallbackArgsWithSource returned nil")
	}
	if enc := extractEncoderFromArgs(got); enc != "libsvtav1" {
		t.Errorf("step 1 encoder = %q, want libsvtav1", enc)
	}

	// Step 2: libsvtav1 has no same-family alternative, and the cross-format
	// transition to libx264 must be refused (TSI-2671).
	if got2 := fallback.PrepareFallbackArgsWithSource(got, "output.mp4", false); got2 != nil {
		t.Errorf("step 2: should return nil (cross-format fallback refused), got %v", got2)
	}
}

func TestEncoderFallback_IsCrossFormatFallback(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		from     string
		to       string
		expected bool
	}{
		{"av1 to h264 is cross-format", "libsvtav1", "libx264", true},
		{"hevc to h264 is cross-format", "libx265", "libx264", true},
		{"vp9 to h264 is cross-format", "libvpx-vp9", "libx264", true},
		{"av1 to av1 is same-format", "libaom-av1", "libsvtav1", false},
		{"h264 to h264 is same-format", "libx264rgb", "libx264", false},
		{"vpx subfamily merge: libvpx to libvpx-vp9", "libvpx", "libvpx-vp9", false},
		{"vp9_vaapi to libvpx-vp9 is same-family", "vp9_vaapi", "libvpx-vp9", false},
		{"unknown source is cross-format (refused)", "unknown_encoder", "libx264", true},
		{"unknown target is cross-format (refused)", "libx265", "unknown_encoder", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.IsCrossFormatFallback(tt.from, tt.to); got != tt.expected {
				t.Errorf("IsCrossFormatFallback(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.expected)
			}
		})
	}
}
func TestEncoderFallback_GetEncoderFamily(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected string
	}{
		{"libvpx is vpx family", "libvpx", "vpx"},
		{"libvpx-vp9 is vpx family", "libvpx-vp9", "vpx"},
		{"vp9_vaapi is vpx family", "vp9_vaapi", "vpx"},
		{"bare vp9 is vpx family", "vp9", "vpx"},
		{"vp8_vaapi is vpx family", "vp8_vaapi", "vpx"},
		{"bare vp8 is vpx family", "vp8", "vpx"},
		{"h264 keeps granular format", "libx264", "h264"},
		{"hevc keeps granular format", "libx265", "hevc"},
		{"av1 keeps granular format", "libsvtav1", "av1"},
		{"unknown encoder is empty", "unknown_encoder", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.GetEncoderFamily(tt.encoder); got != tt.expected {
				t.Errorf("GetEncoderFamily(%q) = %q, want %q", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_CrossFormatChainEntryRefused(t *testing.T) {
	fallback := NewEncoderFallback()

	// A custom cross-format entry must be refused by GetAlternativeSoftwareEncoder.
	fallback.AddSoftwareFallbackChain("libsvtav1", "libx264")
	if got := fallback.GetAlternativeSoftwareEncoder("libsvtav1"); got != "" {
		t.Errorf("GetAlternativeSoftwareEncoder(libsvtav1) = %q, want empty (cross-format refused)", got)
	}

	// Same-family custom entry still works.
	fallback.AddSoftwareFallbackChain("libx264rgb", "libx264")
	if got := fallback.GetAlternativeSoftwareEncoder("libx264rgb"); got != "libx264" {
		t.Errorf("GetAlternativeSoftwareEncoder(libx264rgb) = %q, want libx264", got)
	}
}
func TestEncoderFallback_VPXSubfamilyChainEntryAccepted(t *testing.T) {
	fallback := NewEncoderFallback()

	// libvpx (vp8) -> libvpx-vp9 (vp9) shares the vpx prefix and must be
	// accepted as a same-family transition (TSI-2760).
	fallback.AddSoftwareFallbackChain("libvpx", "libvpx-vp9")
	if got := fallback.GetAlternativeSoftwareEncoder("libvpx"); got != "libvpx-vp9" {
		t.Errorf("GetAlternativeSoftwareEncoder(libvpx) = %q, want libvpx-vp9 (same vpx family)", got)
	}

	// The reverse direction is also within the vpx family.
	fallback.AddSoftwareFallbackChain("libvpx-vp9", "libvpx")
	if got := fallback.GetAlternativeSoftwareEncoder("libvpx-vp9"); got != "libvpx" {
		t.Errorf("GetAlternativeSoftwareEncoder(libvpx-vp9) = %q, want libvpx (same vpx family)", got)
	}
}

func TestEncoderFallback_UnknownFormatChainEntryLogged(t *testing.T) {
	fallback := NewEncoderFallback()
	fallback.AddSoftwareFallbackChain("unknown_encoder", "libx264")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	if got := fallback.GetAlternativeSoftwareEncoder("unknown_encoder"); got != "" {
		t.Fatalf("GetAlternativeSoftwareEncoder(unknown_encoder) = %q, want empty (refused)", got)
	}

	if !strings.Contains(buf.String(), "unknown_encoder") {
		t.Errorf("expected warning to name unknown encoder, got log output: %q", buf.String())
	}
}
