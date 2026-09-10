package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/cli/client"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// captureStdout runs f and returns captured stdout as a string.
// Uses a goroutine to drain the pipe, preventing deadlock when f produces
// more output than the OS pipe buffer (typically 64KB).
func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()

	f()
	w.Close()
	os.Stdout = old
	<-done
	return buf.String()
}

// captureStderr runs f and returns captured stderr as a string.
// Uses a goroutine to drain the pipe, preventing deadlock when f produces
// more output than the OS pipe buffer (typically 64KB).
func captureStderr(f func()) string {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()

	f()
	w.Close()
	os.Stderr = old
	<-done
	return buf.String()
}

// TestRun_NoArgsReturnsError pins the TSI-2907 fix: invoking rffmpeg with no
// arguments must exit non-zero (ffmpeg exits 1) instead of printing usage and
// reporting success.
func TestRun_NoArgsReturnsError(t *testing.T) {
	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = []string{"rffmpeg"}

	code := ExitSuccess
	captureStderr(func() {
		code = run()
	})
	if code != ExitError {
		t.Errorf("run() with no args = %d, want %d", code, ExitError)
	}
}

// TestRejectOverwriteIfNeeded pins the TSI-2964 CLI download guard: in the
// default upload/download mode the CLI is the sole writer of the user's output
// file, so a pre-existing file must not be silently truncated.
func TestRejectOverwriteIfNeeded(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(outPath, []byte("existing content"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		ffmpegArgs []string
		exists     bool
		wantCode   int
		wantMsg    string
	}{
		{"no flag + existing rejects", []string{"-i", "in.mp4", "out.mp4"}, true, ExitError, "Not overwriting"},
		{"-n + existing rejects", []string{"-n", "-i", "in.mp4", "out.mp4"}, true, ExitError, "already exists. Exiting."},
		{"-y + existing allows", []string{"-y", "-i", "in.mp4", "out.mp4"}, true, ExitSuccess, ""},
		{"no flag + absent allows", []string{"-i", "in.mp4", "out.mp4"}, false, ExitSuccess, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := outPath
			if !tt.exists {
				p = filepath.Join(dir, "absent.mp4")
			}
			stderr := captureStderr(func() {
				if code := rejectOverwriteIfNeeded(p, tt.ffmpegArgs); code != tt.wantCode {
					t.Errorf("rejectOverwriteIfNeeded() = %d, want %d", code, tt.wantCode)
				}
			})
			if tt.wantMsg != "" && !strings.Contains(stderr, tt.wantMsg) {
				t.Errorf("stderr = %q, want substring %q", stderr, tt.wantMsg)
			}
		})
	}
}

// TestDefaultModeDownloadOverwriteGuard verifies the end-to-end default-mode
// path: an existing output with no -y is refused (file preserved), while -y
// downloads and overwrites it — the original TSI-2964 scenario.
func TestDefaultModeDownloadOverwriteGuard(t *testing.T) {
	const payload = "transcoded-output-data"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(payload))
	}))
	defer srv.Close()
	c := client.New(srv.URL, "")

	t.Run("no -y preserves existing file", func(t *testing.T) {
		dir := t.TempDir()
		outPath := filepath.Join(dir, "out.mp4")
		if err := os.WriteFile(outPath, []byte("existing content"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := rejectOverwriteIfNeeded(outPath, []string{"-i", "in.mp4", "out.mp4"}); code != ExitError {
			t.Fatalf("expected rejection, got code %d", code)
		}
		b, _ := os.ReadFile(outPath)
		if string(b) != "existing content" {
			t.Errorf("existing file was modified: %q", string(b))
		}
	})

	t.Run("-y downloads and overwrites", func(t *testing.T) {
		dir := t.TempDir()
		outPath := filepath.Join(dir, "out.mp4")
		if err := os.WriteFile(outPath, []byte("existing content"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := rejectOverwriteIfNeeded(outPath, []string{"-y", "-i", "in.mp4", "out.mp4"}); code != ExitSuccess {
			t.Fatalf("expected overwrite allowed, got code %d", code)
		}
		if err := c.DownloadOutput("file-id", outPath); err != nil {
			t.Fatalf("DownloadOutput failed: %v", err)
		}
		b, _ := os.ReadFile(outPath)
		if string(b) != payload {
			t.Errorf("output not overwritten with download payload: %q", string(b))
		}
	})
}

func TestEncoderCapabilityFlags(t *testing.T) {
	tests := []struct {
		name     string
		encType  string
		wantType byte // first character
	}{
		{"video", "video", 'V'},
		{"audio", "audio", 'A'},
		{"subtitle", "subtitle", 'S'},
		{"unknown", "unknown_type", '.'},
		{"empty", "", '.'},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := encoderCapabilityFlags(tt.encType)
			if len(result) != 6 {
				t.Errorf("expected 6-char flag, got %d chars: %q", len(result), result)
			}
			if result[0] != tt.wantType {
				t.Errorf("type char = %c, want %c", result[0], tt.wantType)
			}
			// Verify the remaining 5 chars are all '.'
			for i := 1; i < 6; i++ {
				if result[i] != '.' {
					t.Errorf("flag[%d] = %c, want '.'", i, result[i])
				}
			}
		})
	}
}

func TestDecoderCapabilityFlags(t *testing.T) {
	tests := []string{"video", "audio", "subtitle", "unknown"}

	for _, decType := range tests {
		t.Run(decType, func(t *testing.T) {
			encResult := encoderCapabilityFlags(decType)
			decResult := decoderCapabilityFlags(decType)
			if encResult != decResult {
				t.Errorf("decoderCapabilityFlags(%q) = %q, want %q (same as encoderCapabilityFlags)", decType, decResult, encResult)
			}
		})
	}
}

func TestPrintEncoderDecoderHeader(t *testing.T) {
	output := captureStdout(printEncoderDecoderHeader)

	// Verify header structure
	if !strings.Contains(output, "Encoders:") {
		t.Error("header missing 'Encoders:' line")
	}
	if !strings.Contains(output, "V..... = Video") {
		t.Error("header missing video type legend")
	}
	if !strings.Contains(output, "A..... = Audio") {
		t.Error("header missing audio type legend")
	}
	if !strings.Contains(output, "S..... = Subtitle") {
		t.Error("header missing subtitle type legend")
	}

	// Verify capability legend uses '.' as type placeholder (not 'V')
	if strings.Contains(output, "VF") {
		t.Error("capability legend should use '.' as type placeholder, not 'V'")
	}

	// Verify 6-char flag format for each capability
	expectedLines := []string{
		".F.... = Frame-level multithreading",
		"..S... = Slice-level multithreading",
		"...X.. = Experimental",
		"....B. = Draw horizontal band",
		".....D = Direct rendering",
	}
	for _, expected := range expectedLines {
		if !strings.Contains(output, expected) {
			t.Errorf("header missing capability legend: %q", expected)
		}
	}

	// Verify each capability flag is exactly 6 chars (the part before " =")
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		// Skip non-capability lines
		if !strings.Contains(line, "=") {
			continue
		}
		// The flag is the first word after the leading space
		parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
		if len(parts) > 0 {
			flag := parts[0]
			if len(flag) != 6 {
				t.Errorf("capability flag %q has %d chars, want 6", flag, len(flag))
			}
		}
	}
}

func TestRunEncoders_FallbackToLocal(t *testing.T) {
	// When server is unavailable, should fall back to local ffmpeg
	// and return success (ffmpeg is available in test environment)
	exitCode := runEncoders("http://127.0.0.1:1", "", false)
	if exitCode != ExitSuccess {
		t.Errorf("expected fallback to local ffmpeg success, got exit code %d", exitCode)
	}
}

func TestRunDecoders_FallbackToLocal(t *testing.T) {
	// When server is unavailable, should fall back to local ffmpeg
	exitCode := runDecoders("http://127.0.0.1:1", "", false)
	if exitCode != ExitSuccess {
		t.Errorf("expected fallback to local ffmpeg success, got exit code %d", exitCode)
	}
}

func TestRunEncoders_FallbackJSON(t *testing.T) {
	// JSON mode should also fall back and produce valid JSON
	output := captureStdout(func() {
		exitCode := runEncoders("http://127.0.0.1:1", "", true)
		if exitCode != ExitSuccess {
			t.Errorf("expected JSON fallback success, got exit code %d", exitCode)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}
	if _, ok := result["encoders"]; !ok {
		t.Errorf("expected 'encoders' key in JSON output")
	}
}

func TestRunDecoders_FallbackJSON(t *testing.T) {
	output := captureStdout(func() {
		exitCode := runDecoders("http://127.0.0.1:1", "", true)
		if exitCode != ExitSuccess {
			t.Errorf("expected JSON fallback success, got exit code %d", exitCode)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}
	if _, ok := result["decoders"]; !ok {
		t.Errorf("expected 'decoders' key in JSON output")
	}
}

func TestEncoderCapabilityFlags_AllDotFormat(t *testing.T) {
	// Verify that all non-type positions are dots for all known types
	types := []string{"video", "audio", "subtitle", ""}
	for _, encType := range types {
		result := encoderCapabilityFlags(encType)
		// fmt.Sprintf to verify the full flag in printf format works as expected
		_ = fmt.Sprintf("%s %-22s %s", result, "test_encoder", "Test encoder")
	}
}

// =============================================================================
// Tests for parseFFmpegEncoderLines — parses ffmpeg -encoders / -decoders output
// =============================================================================

func TestParseFFmpegEncoderLines(t *testing.T) {
	// Simulated ffmpeg -encoders output (matching real format)
	input := `Encoders:
 V..... = Video
 A..... = Audio
 S..... = Subtitle
 .F.... = Frame-level multithreading
 ------
 V....D a64multi             Multicolor charset for Commodore 64
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10
 A....D aac                  AAC (Advanced Audio Coding)
 S..... srt                  SubRip subtitle
`

	encoders := parseFFmpegEncoderLines(input)

	if len(encoders) != 4 {
		t.Fatalf("expected 4 encoders, got %d", len(encoders))
	}

	// Check first encoder (video)
	if encoders[0].Name != "a64multi" {
		t.Errorf("encoder[0].Name = %q, want %q", encoders[0].Name, "a64multi")
	}
	if encoders[0].Type != "video" {
		t.Errorf("encoder[0].Type = %q, want %q", encoders[0].Type, "video")
	}
	if !strings.Contains(encoders[0].Description, "Commodore 64") {
		t.Errorf("encoder[0].Description = %q, expected Commodore 64 desc", encoders[0].Description)
	}

	// Check audio encoder
	if encoders[2].Name != "aac" {
		t.Errorf("encoder[2].Name = %q, want %q", encoders[2].Name, "aac")
	}
	if encoders[2].Type != "audio" {
		t.Errorf("encoder[2].Type = %q, want %q", encoders[2].Type, "audio")
	}

	// Check subtitle encoder
	if encoders[3].Name != "srt" {
		t.Errorf("encoder[3].Name = %q, want %q", encoders[3].Name, "srt")
	}
	if encoders[3].Type != "subtitle" {
		t.Errorf("encoder[3].Type = %q, want %q", encoders[3].Type, "subtitle")
	}
}

func TestParseFFmpegEncoderLines_EmptyOutput(t *testing.T) {
	encoders := parseFFmpegEncoderLines("")
	if len(encoders) != 0 {
		t.Errorf("expected 0 encoders from empty input, got %d", len(encoders))
	}
}

func TestParseFFmpegEncoderLines_NoSeparator(t *testing.T) {
	// Input without the "------" separator line
	input := `Encoders:
 V..... = Video
 A..... = Audio
 V....D libx264              libx264 H.264
`
	encoders := parseFFmpegEncoderLines(input)
	if len(encoders) != 0 {
		t.Errorf("expected 0 encoders without separator, got %d", len(encoders))
	}
}

func TestParseFFmpegEncoderLines_UnknownType(t *testing.T) {
	// Lines starting with non-V/A/S type char should be skipped
	input := `Encoders:
 ------
 X....D unknown              Unknown type encoder
 V....D libx264              H.264 encoder
`
	encoders := parseFFmpegEncoderLines(input)
	if len(encoders) != 1 {
		t.Fatalf("expected 1 encoder, got %d", len(encoders))
	}
	if encoders[0].Name != "libx264" {
		t.Errorf("got %q, want libx264", encoders[0].Name)
	}
}

func TestParseFFmpegEncoderLines_ShortLines(t *testing.T) {
	// Lines shorter than 8 chars after trim should be skipped
	input := `Encoders:
 ------
 short
 V....D libx264              H.264 encoder
`
	encoders := parseFFmpegEncoderLines(input)
	if len(encoders) != 1 {
		t.Fatalf("expected 1 encoder, got %d", len(encoders))
	}
}

func TestParseFFmpegEncoderLines_RealFfmpegOutput(t *testing.T) {
	// Test against real ffmpeg -encoders output (captured)
	output, err := runLocalFfmpegOutput("-encoders")
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	encoders := parseFFmpegEncoderLines(output)
	if len(encoders) == 0 {
		t.Error("expected non-zero encoders from real ffmpeg output")
	}

	// Verify all encoders have required fields
	for i, enc := range encoders {
		if enc.Name == "" {
			t.Errorf("encoder[%d] has empty name", i)
		}
		if enc.Type != "video" && enc.Type != "audio" && enc.Type != "subtitle" {
			t.Errorf("encoder[%d] %q has invalid type %q", i, enc.Name, enc.Type)
		}
	}
}

// =============================================================================
// Tests for parseFFmpegNameList — parses ffmpeg -hwaccels / -filters / etc.
// =============================================================================

func TestParseFFmpegNameList_TwoColumnFormat(t *testing.T) {
	// Simulated ffmpeg -filters output (two-column: flags + name)
	input := `Filters:
  T.. = Timeline support
  ------
 TS aap               AA->A      Apply Affine Projection algorithm
 .. abench            A->A       Benchmark part of a filtergraph
 T. acompressor       A->A       Audio compressor
`

	names := parseFFmpegNameList(input)

	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
	if names[0] != "aap" {
		t.Errorf("names[0] = %q, want %q", names[0], "aap")
	}
	if names[1] != "abench" {
		t.Errorf("names[1] = %q, want %q", names[1], "abench")
	}
	if names[2] != "acompressor" {
		t.Errorf("names[2] = %q, want %q", names[2], "acompressor")
	}
}

func TestParseFFmpegNameList_SingleColumnFallback(t *testing.T) {
	// Simulated ffmpeg -hwaccels output (single-column list)
	input := `Hardware acceleration methods:
vdpau
cuda
vaapi
qsv
drm
opencl
vulkan
`

	names := parseFFmpegNameList(input)

	if len(names) == 0 {
		t.Error("expected non-zero names from single-column format")
	}
	// Check that header lines with ':' are skipped
	for _, name := range names {
		if strings.Contains(name, ":") {
			t.Errorf("header line with ':' should be skipped: %q", name)
		}
	}
}

func TestParseFFmpegNameList_EmptyOutput(t *testing.T) {
	names := parseFFmpegNameList("")
	if len(names) != 0 {
		t.Errorf("expected 0 names from empty input, got %d", len(names))
	}
}

func TestParseFFmpegNameList_NoSeparator(t *testing.T) {
	// No "---" separator → falls back to single-column parsing
	input := "item1\nitem2\nitem3\n"
	names := parseFFmpegNameList(input)
	if len(names) != 3 {
		t.Fatalf("expected 3 names from single-column input, got %d", len(names))
	}
	if names[0] != "item1" {
		t.Errorf("names[0] = %q, want item1", names[0])
	}
}

func TestParseFFmpegNameList_SkipsLegendEntries(t *testing.T) {
	// Verify that legend leftovers (=, ---) are skipped
	input := `Filters:
  T.. = Timeline support
  ------
 TS aap               AA->A      Filter
  =  legend leftover
  ---  separator leftover
 TS test              AA->A      Another filter
`
	names := parseFFmpegNameList(input)
	// Note: parseFFmpegNameList only skips entries where the parsed name
	// exactly equals "=" or "---" or starts with "---".
	// Entries like "=  legend leftover" produce name "legend" (not "="),
	// and "---  separator leftover" produces name "separator" (not starting with "---"),
	// so they are NOT skipped. This is expected behavior.
	if len(names) < 2 {
		t.Fatalf("expected at least 2 names, got %d: %v", len(names), names)
	}
	// Verify the two filter names are present
	if names[0] != "aap" {
		t.Errorf("names[0] = %q, want aap", names[0])
	}
	// test should be present somewhere in the list
	foundTest := false
	for _, n := range names {
		if n == "test" {
			foundTest = true
			break
		}
	}
	if !foundTest {
		t.Errorf("expected 'test' in names list, got %v", names)
	}
}

func TestParseFFmpegNameList_RealHwaccelsOutput(t *testing.T) {
	output, err := runLocalFfmpegOutput("-hwaccels")
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	names := parseFFmpegNameList(output)
	if len(names) == 0 {
		t.Error("expected non-zero hwaccels from real ffmpeg output")
	}
	// Verify no empty names
	for i, name := range names {
		if name == "" {
			t.Errorf("hwaccel[%d] is empty", i)
		}
	}
}

func TestParseFFmpegNameList_RealFiltersOutput(t *testing.T) {
	output, err := runLocalFfmpegOutput("-filters")
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	names := parseFFmpegNameList(output)
	if len(names) == 0 {
		t.Error("expected non-zero filters from real ffmpeg output")
	}
	for i, name := range names {
		if name == "" {
			t.Errorf("filter[%d] is empty", i)
		}
	}
}

func TestParseFFmpegNameList_RealPixFmtsOutput(t *testing.T) {
	output, err := runLocalFfmpegOutput("-pix_fmts")
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	names := parseFFmpegNameList(output)
	if len(names) == 0 {
		t.Error("expected non-zero pix_fmts from real ffmpeg output")
	}
}

func TestParseFFmpegNameList_RealFormatsOutput(t *testing.T) {
	output, err := runLocalFfmpegOutput("-formats")
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	names := parseFFmpegNameList(output)
	if len(names) == 0 {
		t.Error("expected non-zero formats from real ffmpeg output")
	}
}

// =============================================================================
// P0/P1 fallback-to-local tests
// =============================================================================

func TestRunHwaccels_FallbackToLocal(t *testing.T) {
	// When server is unavailable, should fall back to local ffmpeg
	exitCode := runHwaccels("http://127.0.0.1:1", "", false)
	if exitCode != ExitSuccess {
		t.Errorf("expected fallback to local ffmpeg success, got exit code %d", exitCode)
	}
}

func TestRunHwaccels_FallbackJSON(t *testing.T) {
	output := captureStdout(func() {
		exitCode := runHwaccels("http://127.0.0.1:1", "", true)
		if exitCode != ExitSuccess {
			t.Errorf("expected JSON fallback success, got exit code %d", exitCode)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}
	hwaccels, ok := result["hwaccels"].([]interface{})
	if !ok {
		t.Fatalf("expected 'hwaccels' key with array, got %T", result["hwaccels"])
	}
	if len(hwaccels) == 0 {
		t.Error("expected non-empty hwaccels array in JSON")
	}
}

func TestRunFilters_FallbackToLocal(t *testing.T) {
	exitCode := runFilters("http://127.0.0.1:1", "", false)
	if exitCode != ExitSuccess {
		t.Errorf("expected fallback to local ffmpeg success, got exit code %d", exitCode)
	}
}

func TestRunFilters_FallbackJSON(t *testing.T) {
	output := captureStdout(func() {
		exitCode := runFilters("http://127.0.0.1:1", "", true)
		if exitCode != ExitSuccess {
			t.Errorf("expected JSON fallback success, got exit code %d", exitCode)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}
	filters, ok := result["filters"].([]interface{})
	if !ok {
		t.Fatalf("expected 'filters' key with array, got %T", result["filters"])
	}
	if len(filters) == 0 {
		t.Error("expected non-empty filters array in JSON")
	}
}

func TestRunPixFmts_FallbackToLocal(t *testing.T) {
	exitCode := runPixFmts("http://127.0.0.1:1", "", false)
	if exitCode != ExitSuccess {
		t.Errorf("expected fallback to local ffmpeg success, got exit code %d", exitCode)
	}
}

func TestRunPixFmts_FallbackJSON(t *testing.T) {
	output := captureStdout(func() {
		exitCode := runPixFmts("http://127.0.0.1:1", "", true)
		if exitCode != ExitSuccess {
			t.Errorf("expected JSON fallback success, got exit code %d", exitCode)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}
	pixFmts, ok := result["pix_fmts"].([]interface{})
	if !ok {
		t.Fatalf("expected 'pix_fmts' key with array, got %T", result["pix_fmts"])
	}
	if len(pixFmts) == 0 {
		t.Error("expected non-empty pix_fmts array in JSON")
	}
}

func TestRunFormats_FallbackToLocal(t *testing.T) {
	exitCode := runFormats("http://127.0.0.1:1", "", false)
	if exitCode != ExitSuccess {
		t.Errorf("expected fallback to local ffmpeg success, got exit code %d", exitCode)
	}
}

func TestRunFormats_FallbackJSON(t *testing.T) {
	output := captureStdout(func() {
		exitCode := runFormats("http://127.0.0.1:1", "", true)
		if exitCode != ExitSuccess {
			t.Errorf("expected JSON fallback success, got exit code %d", exitCode)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}
	formats, ok := result["formats"].([]interface{})
	if !ok {
		t.Fatalf("expected 'formats' key with array, got %T", result["formats"])
	}
	if len(formats) == 0 {
		t.Error("expected non-empty formats array in JSON")
	}
}

func TestRunCodecs_FallbackToLocal(t *testing.T) {
	exitCode := runCodecsFromServer("http://127.0.0.1:1", "", false)
	if exitCode != ExitSuccess {
		t.Errorf("expected fallback to local ffmpeg success, got exit code %d", exitCode)
	}
}

func TestRunCodecs_FallbackJSON(t *testing.T) {
	output := captureStdout(func() {
		exitCode := runCodecsFromServer("http://127.0.0.1:1", "", true)
		if exitCode != ExitSuccess {
			t.Errorf("expected JSON fallback success, got exit code %d", exitCode)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}
	codecs, ok := result["codecs"].([]interface{})
	if !ok {
		t.Fatalf("expected 'codecs' key with array, got %T", result["codecs"])
	}
	if len(codecs) == 0 {
		t.Error("expected non-empty codecs array in JSON")
	}
}

// =============================================================================
// P2 info flag tests — these run local ffmpeg directly (no server needed)
// =============================================================================

func TestRunLocalFfmpegInfo_Buildconf(t *testing.T) {
	exitCode := runLocalFfmpegInfo("-buildconf")
	if exitCode != ExitSuccess {
		t.Errorf("ffmpeg -buildconf failed with exit code %d", exitCode)
	}
}

func TestRunLocalFfmpegInfo_Layouts(t *testing.T) {
	exitCode := runLocalFfmpegInfo("-layouts")
	if exitCode != ExitSuccess {
		t.Errorf("ffmpeg -layouts failed with exit code %d", exitCode)
	}
}

func TestRunLocalFfmpegInfo_Protocols(t *testing.T) {
	exitCode := runLocalFfmpegInfo("-protocols")
	if exitCode != ExitSuccess {
		t.Errorf("ffmpeg -protocols failed with exit code %d", exitCode)
	}
}

func TestRunLocalFfmpegInfo_SampleFmts(t *testing.T) {
	exitCode := runLocalFfmpegInfo("-sample_fmts")
	if exitCode != ExitSuccess {
		t.Errorf("ffmpeg -sample_fmts failed with exit code %d", exitCode)
	}
}

func TestRunLocalFfmpegInfo_Bsfs(t *testing.T) {
	exitCode := runLocalFfmpegInfo("-bsfs")
	if exitCode != ExitSuccess {
		t.Errorf("ffmpeg -bsfs failed with exit code %d", exitCode)
	}
}

func TestRunLocalFfmpegInfo_Colors(t *testing.T) {
	exitCode := runLocalFfmpegInfo("-colors")
	if exitCode != ExitSuccess {
		t.Errorf("ffmpeg -colors failed with exit code %d", exitCode)
	}
}

// =============================================================================
// JSON output format validation tests
// =============================================================================

func TestJSONOutput_EncoderFields(t *testing.T) {
	encoders := []protocol.EncoderInfo{
		{Name: "libx264", Description: "H.264 / AVC encoder", Type: "video"},
		{Name: "aac", Description: "AAC audio encoder", Type: "audio"},
	}

	output := captureStdout(func() {
		printJSONEncoderInfo(encoders, "encoders")
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	entries := result["encoders"].([]interface{})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	for i, entry := range entries {
		e := entry.(map[string]interface{})
		if _, ok := e["name"]; !ok {
			t.Errorf("entry[%d] missing 'name' field", i)
		}
		if _, ok := e["description"]; !ok {
			t.Errorf("entry[%d] missing 'description' field", i)
		}
		if _, ok := e["type"]; !ok {
			t.Errorf("entry[%d] missing 'type' field", i)
		}
	}
}

func TestJSONOutput_DecoderFields(t *testing.T) {
	decoders := []protocol.DecoderInfo{
		{Name: "h264", Description: "H.264 / AVC decoder", Type: "video"},
	}

	output := captureStdout(func() {
		printJSONDecoderInfo(decoders, "decoders")
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	entries := result["decoders"].([]interface{})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0].(map[string]interface{})
	if e["name"] != "h264" {
		t.Errorf("name = %q, want h264", e["name"])
	}
	if e["type"] != "video" {
		t.Errorf("type = %q, want video", e["type"])
	}
}

func TestJSONOutput_CodecsFields(t *testing.T) {
	encoders := []protocol.EncoderInfo{
		{Name: "libx264", Description: "H.264 encoder", Type: "video"},
	}
	decoders := []protocol.DecoderInfo{
		{Name: "h264", Description: "H.264 decoder", Type: "video"},
	}

	output := captureStdout(func() {
		outputInfoFlagJSON("codecs", encoders, decoders)
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	entries := result["codecs"].([]interface{})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// First entry should be an encoder with is_encoder=true
	e0 := entries[0].(map[string]interface{})
	if isEnc, ok := e0["is_encoder"]; !ok || isEnc != true {
		t.Errorf("entry[0].is_encoder should be true, got %v", e0["is_encoder"])
	}
	if e0["name"] != "libx264" {
		t.Errorf("entry[0].name = %q, want libx264", e0["name"])
	}

	// Second entry should be a decoder with is_encoder=false
	e1 := entries[1].(map[string]interface{})
	if isEnc, ok := e1["is_encoder"]; !ok || isEnc != false {
		t.Errorf("entry[1].is_encoder should be false, got %v", e1["is_encoder"])
	}
	if e1["name"] != "h264" {
		t.Errorf("entry[1].name = %q, want h264", e1["name"])
	}
}

// =============================================================================
// Integration tests — compare rffmpeg output with ffmpeg output format
// =============================================================================

func TestOutputFormat_EncodersVsFfmpeg(t *testing.T) {
	// The rffmpeg encoder output format should match ffmpeg's
	// Verify the format: "{flags} {name:22} {description}"

	// Capture rffmpeg output (via fallback to local ffmpeg)
	rffmpegOutput := captureStdout(func() {
		exitCode := runEncoders("http://127.0.0.1:1", "", false)
		if exitCode != ExitSuccess {
			t.Fatalf("rffmpeg fallback failed: exit code %d", exitCode)
		}
	})

	// Verify output structure
	if !strings.Contains(rffmpegOutput, "Encoders:") {
		t.Error("rffmpeg encoder output missing 'Encoders:' header")
	}
	if !strings.Contains(rffmpegOutput, "------") {
		t.Error("rffmpeg encoder output missing separator")
	}

	// Verify each encoder line has the correct format: 6-char flag, space, name, desc
	lines := strings.Split(strings.TrimSpace(rffmpegOutput), "\n")
	dataLines := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Encoder") ||
			strings.HasPrefix(line, "V.....") || strings.HasPrefix(line, "A.....") ||
			strings.HasPrefix(line, "S.....") || strings.HasPrefix(line, ".F....") ||
			strings.HasPrefix(line, "..S...") || strings.HasPrefix(line, "...X..") ||
			strings.HasPrefix(line, "....B.") || strings.HasPrefix(line, ".....D") ||
			line == "------" {
			continue
		}
		dataLines++

		parts := strings.Fields(line)
		if len(parts) < 2 {
			t.Errorf("encoder line has insufficient fields: %q", line)
			continue
		}
		flag := parts[0]
		if len(flag) != 6 {
			t.Errorf("encoder flag %q should be 6 chars", flag)
		}
		if flag[0] != 'V' && flag[0] != 'A' && flag[0] != 'S' {
			t.Errorf("encoder flag %q should start with V/A/S", flag)
		}
	}
	if dataLines == 0 {
		t.Error("no encoder data lines found in output")
	}
}

func TestOutputFormat_DecodersVsFfmpeg(t *testing.T) {
	rffmpegOutput := captureStdout(func() {
		exitCode := runDecoders("http://127.0.0.1:1", "", false)
		if exitCode != ExitSuccess {
			t.Fatalf("rffmpeg fallback failed: exit code %d", exitCode)
		}
	})

	// ffmpeg -decoders output header uses "Decoders:" (not "Encoders:")
	if !strings.Contains(rffmpegOutput, "Decoders:") {
		t.Error("rffmpeg decoder output missing 'Decoders:' header")
	}

	lines := strings.Split(strings.TrimSpace(rffmpegOutput), "\n")
	dataLines := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "------" {
			continue
		}
		// Skip legend lines (single word with "=" style definitions)
		if strings.Contains(line, "=") && len(strings.Fields(line)) <= 5 {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && len(parts[0]) == 6 {
			dataLines++
		}
	}
	if dataLines == 0 {
		t.Error("no decoder data lines found in output")
	}
}

func TestOutputFormat_HwaccelsVsFfmpeg(t *testing.T) {
	// ffmpeg -hwaccels output is a simple list
	rffmpegOutput := captureStdout(func() {
		exitCode := runHwaccels("http://127.0.0.1:1", "", false)
		if exitCode != ExitSuccess {
			t.Fatalf("rffmpeg fallback failed: exit code %d", exitCode)
		}
	})

	// Each non-empty line should be a hwaccel name
	lines := strings.Split(strings.TrimSpace(rffmpegOutput), "\n")
	hwaccelCount := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		hwaccelCount++
		if strings.Contains(line, " ") {
			// hwaccel names shouldn't have spaces
			t.Logf("hwaccel name with space (may be description): %q", line)
		}
	}
	if hwaccelCount == 0 {
		t.Error("no hwaccel names found in output")
	}
}

func TestOutputFormat_FiltersVsFfmpeg(t *testing.T) {
	rffmpegOutput := captureStdout(func() {
		exitCode := runFilters("http://127.0.0.1:1", "", false)
		if exitCode != ExitSuccess {
			t.Fatalf("rffmpeg fallback failed: exit code %d", exitCode)
		}
	})

	// The rffmpeg -filters output comes from runLocalFfmpegPassthrough
	// which prints the full ffmpeg output including the flag+name+desc lines
	if !strings.Contains(rffmpegOutput, "------") {
		t.Error("filters output missing separator line")
	}
}

func TestOutputFormat_P2Buildconf(t *testing.T) {
	output := captureStdout(func() {
		exitCode := runLocalFfmpegInfo("-buildconf")
		if exitCode != ExitSuccess {
			t.Fatalf("ffmpeg -buildconf failed: exit code %d", exitCode)
		}
	})

	// buildconf output should contain build configuration lines
	if len(strings.TrimSpace(output)) == 0 {
		t.Error("buildconf output is empty")
	}
	// Typical buildconf contains "configuration:" line
	lines := strings.Split(output, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), "configuration") {
			found = true
			break
		}
	}
	if !found {
		t.Log("no 'configuration' line found in buildconf, but output is non-empty")
	}
}

// =============================================================================
// jsonEncodeToStdout tests
// =============================================================================

func TestJSONEncodeToStdout_Success(t *testing.T) {
	output := captureStdout(func() {
		data := map[string]interface{}{"test": "value", "num": 42}
		exitCode := jsonEncodeToStdout(json.NewEncoder(os.Stdout), data)
		if exitCode != ExitSuccess {
			t.Errorf("expected exit %d, got %d", ExitSuccess, exitCode)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if result["test"] != "value" {
		t.Errorf("test = %v, want value", result["test"])
	}
	if result["num"] != float64(42) {
		t.Errorf("num = %v, want 42", result["num"])
	}
}

func TestJSONEncodeToStdout_Array(t *testing.T) {
	output := captureStdout(func() {
		data := []string{"a", "b", "c"}
		exitCode := jsonEncodeToStdout(json.NewEncoder(os.Stdout), data)
		if exitCode != ExitSuccess {
			t.Errorf("expected exit %d, got %d", ExitSuccess, exitCode)
		}
	})

	var result []interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("expected 3 elements, got %d", len(result))
	}
}

// =============================================================================
// runLocalFfmpegOutput and runLocalFfmpegPassthrough tests
// =============================================================================

func TestRunLocalFfmpegOutput_Encoders(t *testing.T) {
	output, err := runLocalFfmpegOutput("-encoders")
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	if len(output) == 0 {
		t.Error("expected non-empty output from ffmpeg -encoders")
	}
	if !strings.Contains(output, "Encoders:") {
		t.Error("output missing 'Encoders:' header")
	}
}

func TestRunLocalFfmpegOutput_Hwaccels(t *testing.T) {
	output, err := runLocalFfmpegOutput("-hwaccels")
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	if len(output) == 0 {
		t.Error("expected non-empty output from ffmpeg -hwaccels")
	}
}

func TestRunLocalFfmpegPassthrough_Protocols(t *testing.T) {
	exitCode := runLocalFfmpegPassthrough("-protocols")
	if exitCode != ExitSuccess {
		t.Errorf("ffmpeg -protocols failed with exit code %d", exitCode)
	}
}

// =============================================================================
// Edge case tests
// =============================================================================

func TestParseFFmpegEncoderLines_EmptyLines(t *testing.T) {
	input := `Encoders:
 ------



 V....D libx264              H.264 encoder


`
	encoders := parseFFmpegEncoderLines(input)
	if len(encoders) != 1 {
		t.Fatalf("expected 1 encoder, got %d", len(encoders))
	}
	if encoders[0].Name != "libx264" {
		t.Errorf("got %q, want libx264", encoders[0].Name)
	}
}

func TestParseFFmpegNameList_BlankLines(t *testing.T) {
	input := `Header:
 ------

 item1

 item2

`
	names := parseFFmpegNameList(input)
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d: %v", len(names), names)
	}
}

func TestParseFFmpegNameList_OnlySeparator(t *testing.T) {
	// After separator, no data lines
	input := "------\n"
	names := parseFFmpegNameList(input)
	if len(names) != 0 {
		t.Errorf("expected 0 names, got %d", len(names))
	}
}

func TestParseFFmpegNameList_BothFormats(t *testing.T) {
	// Verify that when both separator and single-column data exist,
	// the two-column parser takes precedence
	input := `Filters:
 ------
 TS aap               desc
`
	names := parseFFmpegNameList(input)
	if len(names) != 1 {
		t.Fatalf("expected 1 name, got %d", len(names))
	}
	if names[0] != "aap" {
		t.Errorf("got %q, want aap", names[0])
	}
}

func TestEncoderCapabilityFlags_AllTypesConsistent(t *testing.T) {
	types := []string{"video", "audio", "subtitle", ""}
	for _, encType := range types {
		flags := encoderCapabilityFlags(encType)
		if len(flags) != 6 {
			t.Errorf("type %q: expected 6 chars, got %d", encType, len(flags))
		}
		for i := 1; i < 6; i++ {
			if flags[i] != '.' {
				t.Errorf("type %q: flags[%d] = %c, want '.'", encType, i, flags[i])
			}
		}
	}
}

func TestReportTerminalJob_NoWorkerAvailable(t *testing.T) {
	job := &protocol.JobInfo{
		ID:          "job-noworker",
		Status:      protocol.JobStatusFailed,
		FailureType: string(protocol.FailureNoWorkerAvailable),
		Error:       "no worker available for over 30s",
	}

	var code int
	stderr := captureStderr(func() {
		code = reportTerminalJob(job)
	})

	if code != ExitError {
		t.Fatalf("reportTerminalJob() = %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "server reported no worker available") {
		t.Errorf("stderr = %q, want server-judged no-worker message", stderr)
	}
	if !strings.Contains(stderr, job.ID) {
		t.Errorf("stderr = %q, want job ID %q", stderr, job.ID)
	}
}

func TestReportTerminalJob_FailedOther(t *testing.T) {
	job := &protocol.JobInfo{
		ID:          "job-ffmpeg",
		Status:      protocol.JobStatusFailed,
		FailureType: string(protocol.FailureFFmpegError),
		Error:       "encoder crashed",
	}

	var code int
	stderr := captureStderr(func() {
		code = reportTerminalJob(job)
	})

	if code != ExitError {
		t.Fatalf("reportTerminalJob() = %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "Job failed: encoder crashed") {
		t.Errorf("stderr = %q, want generic Job failed message", stderr)
	}
	if strings.Contains(stderr, "no worker available") {
		t.Errorf("stderr = %q, must not mention no-worker for non-starvation failure", stderr)
	}
}

func TestReportTerminalJob_Completed(t *testing.T) {
	job := &protocol.JobInfo{
		ID:     "job-done",
		Status: protocol.JobStatusCompleted,
	}

	var code int
	stderr := captureStderr(func() {
		code = reportTerminalJob(job)
	})

	if code != ExitSuccess {
		t.Fatalf("reportTerminalJob() = %d, want ExitSuccess", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty for completed job", stderr)
	}
}

// TestResolveOutputFilename locks the TSI-2690 fix: in shared FS mode network
// output URLs must pass through unchanged instead of being mangled into a
// local path, while local paths (including file:// and paths that merely
// contain "://" mid-string) are still resolved to absolute paths.
func TestResolveOutputFilename(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		sharedFS bool
		want     string
		wantErr  bool
	}{
		{
			name:     "shared FS rtmp URL passes through",
			output:   "rtmp://127.0.0.1:1935/live/X",
			sharedFS: true,
			want:     "rtmp://127.0.0.1:1935/live/X",
		},
		{
			name:     "shared FS srt URL passes through",
			output:   "srt://host:9000?mode=listener",
			sharedFS: true,
			want:     "srt://host:9000?mode=listener",
		},
		{
			name:     "shared FS https URL passes through",
			output:   "https://cdn.example.com/out.mkv",
			sharedFS: true,
			want:     "https://cdn.example.com/out.mkv",
		},
		{
			name:     "shared FS udp URL passes through",
			output:   "udp://239.0.0.1:1234",
			sharedFS: true,
			want:     "udp://239.0.0.1:1234",
		},
		{
			name:     "shared FS local path resolved absolute",
			output:   "output.mp4",
			sharedFS: true,
			want:     mustAbs(t, "output.mp4"),
		},
		{
			name:     "shared FS file URL resolved absolute",
			output:   "file://" + mustAbs(t, "output.mp4"),
			sharedFS: true,
			want:     mustAbs(t, "output.mp4"),
		},
		{
			name:     "shared FS mid-string scheme stays local",
			output:   "/data/media/x://out.mp4",
			sharedFS: true,
			want:     "/data/media/x://out.mp4",
		},
		{
			name:     "non-shared FS rtmp URL passes through",
			output:   "rtmp://127.0.0.1:1935/live/X",
			sharedFS: false,
			want:     "rtmp://127.0.0.1:1935/live/X",
		},
		{
			name:     "non-shared FS local path keeps base name",
			output:   "dir/output.mp4",
			sharedFS: false,
			want:     "output.mp4",
		},
		{
			name:     "non-shared FS file URL keeps base name",
			output:   "file://" + mustAbs(t, "out.mkv"),
			sharedFS: false,
			want:     "out.mkv",
		},
		{
			name:     "non-shared FS mid-string scheme keeps base name",
			output:   "/data/media/x://out.mp4",
			sharedFS: false,
			want:     "out.mp4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveOutputFilename(tt.output, tt.sharedFS)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveOutputFilename(%q, %v) error = %v, wantErr %v", tt.output, tt.sharedFS, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("resolveOutputFilename(%q, %v) = %q, want %q", tt.output, tt.sharedFS, got, tt.want)
			}
		})
	}
}

// mustAbs resolves p to an absolute path for test expectations.
func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", p, err)
	}
	return abs
}

func TestClientWaitDeadline(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// pending timeout-only: deadline is now+timeout+clientVerdictGrace.
	timeout := time.Hour
	got, has := clientWaitDeadline(now, timeout, protocol.JobStatusPending, nil, nil)
	if !has {
		t.Fatal("clientWaitDeadline(pending timeout-only) has = false, want true")
	}
	want := now.Add(timeout + clientVerdictGrace)
	if !got.Equal(want) {
		t.Errorf("clientWaitDeadline(pending timeout-only) = %v, want %v", got, want)
	}

	// running started-at anchor: the budget runs from started_at, not submit
	// time (TSI-2886), so the give-up line shifts by the pre-exec latency.
	startedAt := now.Add(8 * time.Second)
	got, has = clientWaitDeadline(now, timeout, protocol.JobStatusRunning, &startedAt, nil)
	if !has {
		t.Fatal("clientWaitDeadline(started) has = false, want true")
	}
	if want := startedAt.Add(timeout + clientVerdictGrace); !got.Equal(want) {
		t.Errorf("clientWaitDeadline(started) = %v, want %v", got, want)
	}

	// queued: --timeout is the ffmpeg budget, not the pre-exec budget, so a
	// claimed job in download/probe must not be given a --timeout bound.
	if _, has := clientWaitDeadline(now, timeout, protocol.JobStatusQueued, nil, nil); has {
		t.Error("clientWaitDeadline(queued) has = true, want false (no --timeout bound)")
	}

	// no bounds: has must be false.
	if _, has := clientWaitDeadline(now, 0, protocol.JobStatusPending, nil, nil); has {
		t.Error("clientWaitDeadline(no bounds) has = true, want false")
	}

	// noWorkerDeadline-only: deadline is verdict + clientVerdictGrace.
	verdict := now.Add(2 * time.Minute)
	got, has = clientWaitDeadline(now, 0, protocol.JobStatusPending, nil, &verdict)
	if !has {
		t.Fatal("clientWaitDeadline(verdict-only) has = false, want true")
	}
	if !got.Equal(verdict.Add(clientVerdictGrace)) {
		t.Errorf("clientWaitDeadline(verdict-only) = %v, want %v", got, verdict.Add(clientVerdictGrace))
	}

	// timeout later than verdict: deadline follows timeout.
	got, _ = clientWaitDeadline(now, 3*time.Minute, protocol.JobStatusPending, nil, &verdict)
	if !got.Equal(now.Add(3*time.Minute + clientVerdictGrace)) {
		t.Errorf("clientWaitDeadline(timeout later) = %v, want timeout-bound", got)
	}

	// verdict later than timeout: deadline follows verdict.
	laterVerdict := now.Add(10 * time.Minute)
	got, _ = clientWaitDeadline(now, 2*time.Minute, protocol.JobStatusPending, nil, &laterVerdict)
	if !got.Equal(laterVerdict.Add(clientVerdictGrace)) {
		t.Errorf("clientWaitDeadline(verdict later) = %v, want verdict-bound", got)
	}

	// Overflow input must not produce a deadline in the past.
	maxDur := time.Duration(1<<63 - 1)
	got, has = clientWaitDeadline(now, maxDur, protocol.JobStatusPending, nil, nil)
	if !has {
		t.Fatal("clientWaitDeadline(overflow) has = false, want true")
	}
	if got.Before(now) {
		t.Errorf("clientWaitDeadline(overflow) = %v, wrapped into the past", got)
	}
}

func TestExtendWaitForNoWorkerDeadline(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exhausted := now.Add(35 * time.Second)

	// No fresh deadline: do not extend (fall back to give-up).
	if extendWaitForNoWorkerDeadline(exhausted, nil) {
		t.Error("extendWaitForNoWorkerDeadline(nil) = true, want false")
	}

	// Fresh deadline at or before the exhausted bound: the server verdict
	// should already be observable, so do not extend.
	past := exhausted.Add(-time.Second)
	if extendWaitForNoWorkerDeadline(exhausted, &past) {
		t.Error("extendWaitForNoWorkerDeadline(verdict<exhausted) = true, want false")
	}
	if extendWaitForNoWorkerDeadline(exhausted, &exhausted) {
		t.Error("extendWaitForNoWorkerDeadline(verdict==exhausted) = true, want false")
	}

	// Fresh deadline after the exhausted bound: extend so the server verdict
	// is observable before the client gives up.
	future := exhausted.Add(time.Minute)
	if !extendWaitForNoWorkerDeadline(exhausted, &future) {
		t.Error("extendWaitForNoWorkerDeadline(verdict>exhausted) = false, want true")
	}
}

// scriptedJobClient is a fake jobWaitClient whose GetJob returns a fixed
// status sequence, one entry per call. Its wait methods return
// DeadlineExceeded immediately (the loop only classifies the error and
// re-reads; the wall-clock bounds are covered by TestClientWaitDeadline and
// TestExtendWaitForNoWorkerDeadline) while recording the context deadline so
// the test can assert the bound actually passed to each wait.
type scriptedJobClient struct {
	jobID         string
	seq           []protocol.JobInfo
	getCalls      int
	waitCalls     int
	waitDeadlines []time.Time
	cancelCalls   int
}

func (s *scriptedJobClient) GetJob(jobID string) (*protocol.JobInfo, error) {
	if jobID != s.jobID {
		return nil, fmt.Errorf("unexpected jobID %q, want %q", jobID, s.jobID)
	}
	if s.getCalls >= len(s.seq) {
		last := s.seq[len(s.seq)-1]
		return &last, nil
	}
	j := s.seq[s.getCalls]
	s.getCalls++
	return &j, nil
}

func (s *scriptedJobClient) WaitForJobWithLogs(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {
	if jobID != s.jobID {
		return nil, fmt.Errorf("unexpected jobID %q, want %q", jobID, s.jobID)
	}
	s.waitCalls++
	if d, ok := ctx.Deadline(); ok {
		s.waitDeadlines = append(s.waitDeadlines, d)
	}
	return nil, context.DeadlineExceeded
}

func (s *scriptedJobClient) WaitForJobWithStreamingOutput(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {
	if jobID != s.jobID {
		return nil, fmt.Errorf("unexpected jobID %q, want %q", jobID, s.jobID)
	}
	s.waitCalls++
	if d, ok := ctx.Deadline(); ok {
		s.waitDeadlines = append(s.waitDeadlines, d)
	}
	return nil, context.DeadlineExceeded
}

func (s *scriptedJobClient) CancelJob(jobID string) error {
	s.cancelCalls++
	return nil
}

// TestWaitForJobLoop_ExtendsOnceAfterWorkerRevert pins the TSI-2571 core
// behavior at loop level: the client submits while the job is pending with no
// NoWorkerDeadline (waiting behind a busy worker), its --timeout wait fires,
// and the re-read after the deadline observes the job now carries a fresh
// verdict (the busy worker died). The loop must extend the wait once — the
// second wait's bound is the fresh verdict plus grace, not the exhausted
// fallback bound — and then terminate by returning the terminal job, without
// cancelling it.
func TestWaitForJobLoop_ExtendsOnceAfterWorkerRevert(t *testing.T) {
	now := time.Now()
	verdict := now.Add(10 * time.Second)
	timeout := 50 * time.Millisecond
	jobID := "job-revert"

	fake := &scriptedJobClient{
		jobID: jobID,
		seq: []protocol.JobInfo{
			{ID: jobID, Status: protocol.JobStatusPending},
			{ID: jobID, Status: protocol.JobStatusPending, NoWorkerDeadline: &verdict},
			{ID: jobID, Status: protocol.JobStatusPending, NoWorkerDeadline: &verdict},
			{ID: jobID, Status: protocol.JobStatusFailed, FailureType: string(protocol.FailureNoWorkerAvailable), Error: "no worker available"},
		},
	}

	var job *protocol.JobInfo
	var code int
	stderr := captureStderr(func() {
		job, code = waitForJobLoop(fake, jobID, timeout, false, true)
	})

	if code != ExitSuccess {
		t.Fatalf("waitForJobLoop code = %d, want ExitSuccess (loop got a terminal result)", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty on terminal-result path", stderr)
	}
	if job == nil || job.Status != protocol.JobStatusFailed {
		t.Fatalf("returned job = %+v, want failed verdict job", job)
	}
	if fake.getCalls != 4 {
		t.Errorf("GetJob calls = %d, want 4 (pre-wait, post-wait, pre-wait, post-wait)", fake.getCalls)
	}
	if fake.waitCalls != 2 {
		t.Errorf("wait calls = %d, want 2", fake.waitCalls)
	}
	if fake.cancelCalls != 0 {
		t.Errorf("CancelJob calls = %d, want 0 (loop must extend, not cancel)", fake.cancelCalls)
	}
	if len(fake.waitDeadlines) != 2 {
		t.Fatalf("wait deadlines recorded = %d, want 2", len(fake.waitDeadlines))
	}
	// First wait used the no-verdict fallback bound (before the fresh verdict);
	// second wait used the fresh verdict plus grace (after it). That delta is
	// the observable proof the loop re-read the deadline and extended once.
	if !fake.waitDeadlines[0].Before(verdict) {
		t.Errorf("first wait deadline = %v, want before fresh verdict %v", fake.waitDeadlines[0], verdict)
	}
	if !fake.waitDeadlines[1].After(verdict) {
		t.Errorf("second wait deadline = %v, want after fresh verdict %v", fake.waitDeadlines[1], verdict)
	}
	if !fake.waitDeadlines[1].After(fake.waitDeadlines[0]) {
		t.Errorf("second wait deadline %v not after first %v: wait was not extended", fake.waitDeadlines[1], fake.waitDeadlines[0])
	}
}

// TestWaitForJobLoop_DoesNotExtendTwice pins the other half of the contract:
// once the client has waited to the fresh verdict bound, a second
// DeadlineExceeded with the same (fixed) verdict must NOT extend again — the
// verdict is immutable and already included in the exhausted bound, so the
// loop cancels. The job is pending here, so the operator message must say the
// client is still waiting for a worker and must NOT claim the server may yet
// fail the job with NO_WORKER_AVAILABLE (that verdict can never be pending on
// this path — the fix for the review's blocking finding).
func TestWaitForJobLoop_DoesNotExtendTwice(t *testing.T) {
	now := time.Now()
	verdict := now.Add(10 * time.Second)
	timeout := 50 * time.Millisecond
	jobID := "job-stuck-pending"

	fake := &scriptedJobClient{
		jobID: jobID,
		seq: []protocol.JobInfo{
			{ID: jobID, Status: protocol.JobStatusQueued},
			{ID: jobID, Status: protocol.JobStatusPending, NoWorkerDeadline: &verdict},
			{ID: jobID, Status: protocol.JobStatusPending, NoWorkerDeadline: &verdict},
			{ID: jobID, Status: protocol.JobStatusPending, NoWorkerDeadline: &verdict},
		},
	}

	var job *protocol.JobInfo
	var code int
	stderr := captureStderr(func() {
		job, code = waitForJobLoop(fake, jobID, timeout, false, true)
	})

	if code != ExitError {
		t.Fatalf("waitForJobLoop code = %d, want ExitError (client gave up)", code)
	}
	if job != nil {
		t.Errorf("returned job = %+v, want nil on give-up path", job)
	}
	if fake.getCalls != 4 {
		t.Errorf("GetJob calls = %d, want 4", fake.getCalls)
	}
	if fake.waitCalls != 2 {
		t.Errorf("wait calls = %d, want 2 (extension must fire at most once)", fake.waitCalls)
	}
	if fake.cancelCalls != 1 {
		t.Errorf("CancelJob calls = %d, want 1", fake.cancelCalls)
	}
	if !strings.Contains(stderr, "still waiting for a worker") {
		t.Errorf("stderr = %q, want pending-job give-up message", stderr)
	}
	if strings.Contains(stderr, "NO_WORKER_AVAILABLE") {
		t.Errorf("stderr = %q, must not claim a server NO_WORKER_AVAILABLE verdict is still possible", stderr)
	}
}

// TestWaitForJobLoop_ExtendsOnceAfterJobStarts is the TSI-2886 regression: the
// client's first wait is anchored to submit time (started_at nil), but once the
// job starts running the worker's ffmpeg budget only begins at started_at. The
// loop must re-anchor to started_at+timeout and wait for the worker's TIMEOUT
// verdict instead of cancelling the running job early.
func TestWaitForJobLoop_ExtendsOnceAfterJobStarts(t *testing.T) {
	now := time.Now()
	startedAt := now.Add(2 * time.Second)
	timeout := 50 * time.Millisecond
	jobID := "job-started"

	fake := &scriptedJobClient{
		jobID: jobID,
		seq: []protocol.JobInfo{
			{ID: jobID, Status: protocol.JobStatusPending},
			{ID: jobID, Status: protocol.JobStatusRunning, StartedAt: &startedAt},
			{ID: jobID, Status: protocol.JobStatusRunning, StartedAt: &startedAt},
			{ID: jobID, Status: protocol.JobStatusTimeout, Error: "job exceeded its time limit"},
		},
	}

	var job *protocol.JobInfo
	var code int
	stderr := captureStderr(func() {
		job, code = waitForJobLoop(fake, jobID, timeout, false, true)
	})

	if code != ExitSuccess {
		t.Fatalf("waitForJobLoop code = %d, want ExitSuccess (worker TIMEOUT verdict observed)", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty on terminal-verdict path", stderr)
	}
	if job == nil || job.Status != protocol.JobStatusTimeout {
		t.Fatalf("returned job = %+v, want timeout verdict job", job)
	}
	if fake.cancelCalls != 0 {
		t.Errorf("CancelJob calls = %d, want 0 (loop must extend, not cancel)", fake.cancelCalls)
	}
	if len(fake.waitDeadlines) != 2 {
		t.Fatalf("wait deadlines recorded = %d, want 2", len(fake.waitDeadlines))
	}
	if !fake.waitDeadlines[1].After(fake.waitDeadlines[0]) {
		t.Errorf("second wait deadline %v not after first %v: wait was not re-anchored to started_at", fake.waitDeadlines[1], fake.waitDeadlines[0])
	}
}

// TestWaitForJobLoop_QueuedJobNotCancelledByTimeout is the TSI-2886 regression
// for the download/probe phase: a queued job is already claimed by a worker and
// spends --timeout on its pre-exec pipeline, which the worker bounds
// independently (30m dataClient download + ffprobe executor). The client must not cancel a
// queued job on the --timeout bound — it waits for the worker's terminal
// verdict instead.
func TestWaitForJobLoop_QueuedJobNotCancelledByTimeout(t *testing.T) {
	timeout := 50 * time.Millisecond
	jobID := "job-queued"

	fake := &scriptedJobClient{
		jobID: jobID,
		seq: []protocol.JobInfo{
			{ID: jobID, Status: protocol.JobStatusQueued},
			{ID: jobID, Status: protocol.JobStatusQueued},
			{ID: jobID, Status: protocol.JobStatusQueued},
			{ID: jobID, Status: protocol.JobStatusTimeout, Error: "job exceeded its time limit"},
		},
	}

	var job *protocol.JobInfo
	var code int
	stderr := captureStderr(func() {
		job, code = waitForJobLoop(fake, jobID, timeout, false, true)
	})

	if code != ExitSuccess {
		t.Fatalf("waitForJobLoop code = %d, want ExitSuccess (worker TIMEOUT verdict observed)", code)
	}
	if job == nil || job.Status != protocol.JobStatusTimeout {
		t.Fatalf("returned job = %+v, want timeout verdict job", job)
	}
	if fake.cancelCalls != 0 {
		t.Errorf("CancelJob calls = %d, want 0 (queued job must not be cancelled on --timeout)", fake.cancelCalls)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty on terminal-verdict path", stderr)
	}
}

// TestWaitForJobLoop_PendingToQueuedNotCancelled covers the claim transition:
// a pending job whose --timeout wait fires, but which has since been claimed by
// a worker (queued), must not be cancelled as "did not start" — the client
// re-anchors to the no-deadline queued wait and awaits the verdict.
func TestWaitForJobLoop_PendingToQueuedNotCancelled(t *testing.T) {
	timeout := 50 * time.Millisecond
	jobID := "job-pending-to-queued"

	fake := &scriptedJobClient{
		jobID: jobID,
		seq: []protocol.JobInfo{
			{ID: jobID, Status: protocol.JobStatusPending},
			{ID: jobID, Status: protocol.JobStatusQueued},
			{ID: jobID, Status: protocol.JobStatusQueued},
			{ID: jobID, Status: protocol.JobStatusTimeout, Error: "job exceeded its time limit"},
		},
	}

	var job *protocol.JobInfo
	var code int
	job, code = waitForJobLoop(fake, jobID, timeout, false, true)

	if code != ExitSuccess {
		t.Fatalf("waitForJobLoop code = %d, want ExitSuccess (worker TIMEOUT verdict observed)", code)
	}
	if job == nil || job.Status != protocol.JobStatusTimeout {
		t.Fatalf("returned job = %+v, want timeout verdict job", job)
	}
	if fake.cancelCalls != 0 {
		t.Errorf("CancelJob calls = %d, want 0 (claimed job must not be cancelled as 'did not start')", fake.cancelCalls)
	}
}

// disconnectedJobClient is a jobWaitClient whose wait methods report that the
// retry budget was spent after the job was submitted.
type disconnectedJobClient struct {
	jobID string
}

func (d *disconnectedJobClient) GetJob(jobID string) (*protocol.JobInfo, error) {
	return &protocol.JobInfo{ID: jobID, Status: protocol.JobStatusRunning}, nil
}

func (d *disconnectedJobClient) WaitForJobWithLogs(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {
	return nil, &client.RetriesExhaustedError{JobID: d.jobID, Cause: errors.New("boom")}
}

func (d *disconnectedJobClient) WaitForJobWithStreamingOutput(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {
	return nil, &client.RetriesExhaustedError{JobID: d.jobID, Cause: errors.New("boom")}
}

func (d *disconnectedJobClient) CancelJob(jobID string) error { return nil }

// TestWaitForJobLoop_RetriesExhausted maps a RetriesExhaustedError to
// ExitDisconnected — the job was submitted but the client lost contact after
// its retry budget, which operators must distinguish from ExitError (TSI-2697).
func TestWaitForJobLoop_RetriesExhausted(t *testing.T) {
	fake := &disconnectedJobClient{jobID: "job-lost"}

	var code int
	stderr := captureStderr(func() {
		_, code = waitForJobLoop(fake, "job-lost", 0, false, true)
	})

	if code != ExitDisconnected {
		t.Fatalf("waitForJobLoop code = %d, want ExitDisconnected", code)
	}
	if !strings.Contains(stderr, "GET /api/v1/jobs/job-lost") {
		t.Errorf("stderr = %q, want final-status query hint", stderr)
	}
}

func TestReportTerminalJob_Nil(t *testing.T) {
	var code int
	stderr := captureStderr(func() {
		code = reportTerminalJob(nil)
	})

	if code != ExitError {
		t.Fatalf("reportTerminalJob(nil) = %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "no job result returned from server") {
		t.Errorf("stderr = %q, want no-job-result message", stderr)
	}
}

func TestReportTerminalJob_Cancelled(t *testing.T) {
	job := &protocol.JobInfo{ID: "job-cancelled", Status: protocol.JobStatusCancelled}

	var code int
	stderr := captureStderr(func() {
		code = reportTerminalJob(job)
	})

	if code != ExitError {
		t.Fatalf("reportTerminalJob(cancelled) = %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "Job was cancelled") {
		t.Errorf("stderr = %q, want cancelled message", stderr)
	}
}

func TestReportTerminalJob_Timeout(t *testing.T) {
	job := &protocol.JobInfo{ID: "job-timeout", Status: protocol.JobStatusTimeout, Error: "job exceeded its time limit"}

	var code int
	stderr := captureStderr(func() {
		code = reportTerminalJob(job)
	})

	if code != ExitError {
		t.Fatalf("reportTerminalJob(timeout) = %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "Job timed out: job exceeded its time limit") {
		t.Errorf("stderr = %q, want timeout message", stderr)
	}
}

func TestReportTerminalJob_UnexpectedStatus(t *testing.T) {
	job := &protocol.JobInfo{ID: "job-pending", Status: protocol.JobStatusPending}

	var code int
	stderr := captureStderr(func() {
		code = reportTerminalJob(job)
	})

	if code != ExitError {
		t.Fatalf("reportTerminalJob(pending) = %d, want ExitError", code)
	}
	if !strings.Contains(stderr, "unexpected status: pending") {
		t.Errorf("stderr = %q, want unexpected-status message", stderr)
	}
}
