package worker

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// CodecType represents the type of codec (Video, Audio, Subtitle)
type CodecType string

const (
	CodecTypeVideo    CodecType = "V"
	CodecTypeAudio    CodecType = "A"
	CodecTypeSubtitle CodecType = "S"
)

// CodecInfo represents information about an encoder or decoder
type CodecInfo struct {
	Name        string    // Codec name (e.g., "libx264", "h264_nvenc")
	Description string    // Codec description
	Type        CodecType // Type: V (Video), A (Audio), S (Subtitle)
	IsHW        bool      // True if this is a hardware-accelerated codec
}

// FFmpegInfo contains all detected FFmpeg information
type FFmpegInfo struct {
	Version  string      // FFmpeg version string
	Encoders []CodecInfo // List of available encoders
	Decoders []CodecInfo // List of available decoders

	// P0/P1 raw text outputs for info flags
	Hwaccels string // Raw text from ffmpeg -hwaccels
	Codecs   string // Raw text from ffmpeg -codecs
	Filters  string // Raw text from ffmpeg -filters
	PixFmts  string // Raw text from ffmpeg -pix_fmts
	Formats  string // Raw text from ffmpeg -formats
}

// FFmpegProbe probes FFmpeg for available encoders, decoders, and version info
type FFmpegProbe struct {
	ffmpegPath string
	timeout    time.Duration
}

// NewFFmpegProbe creates a new FFmpeg probe
func NewFFmpegProbe(ffmpegPath string) *FFmpegProbe {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	return &FFmpegProbe{
		ffmpegPath: ffmpegPath,
		timeout:    30 * time.Second, // Default timeout for probe commands
	}
}

// ValidatePath checks if the ffmpeg executable exists and is executable
func (p *FFmpegProbe) ValidatePath() error {
	// Use exec.LookPath to find the executable in PATH or validate absolute path
	path, err := exec.LookPath(p.ffmpegPath)
	if err != nil {
		return fmt.Errorf("ffmpeg executable not found: %q: %w", p.ffmpegPath, err)
	}
	// Update the path to the resolved absolute path
	p.ffmpegPath = path
	return nil
}

// Probe runs all probes and returns complete FFmpeg information
func (p *FFmpegProbe) Probe(ctx context.Context) (*FFmpegInfo, error) {
	// Validate ffmpeg path first
	if err := p.ValidatePath(); err != nil {
		return nil, err
	}

	info := &FFmpegInfo{}

	// Get version
	version, err := p.GetVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get ffmpeg version: %w", err)
	}
	info.Version = version

	// Get encoders
	encoders, err := p.GetEncoders(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get encoders: %w", err)
	}
	info.Encoders = encoders

	// Get decoders
	decoders, err := p.GetDecoders(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get decoders: %w", err)
	}
	info.Decoders = decoders

	// Get hwaccels (P0)
	hwaccels, err := p.getRawInfo(ctx, "-hwaccels")
	if err != nil {
		return nil, fmt.Errorf("failed to get hwaccels: %w", err)
	}
	info.Hwaccels = hwaccels

	// Get codecs (P1)
	codecs, err := p.getRawInfo(ctx, "-codecs")
	if err != nil {
		return nil, fmt.Errorf("failed to get codecs: %w", err)
	}
	info.Codecs = codecs

	// Get filters (P1)
	filters, err := p.getRawInfo(ctx, "-filters")
	if err != nil {
		return nil, fmt.Errorf("failed to get filters: %w", err)
	}
	info.Filters = filters

	// Get pix_fmts (P1)
	pixFmts, err := p.getRawInfo(ctx, "-pix_fmts")
	if err != nil {
		return nil, fmt.Errorf("failed to get pix_fmts: %w", err)
	}
	info.PixFmts = pixFmts

	// Get formats (P1)
	formats, err := p.getRawInfo(ctx, "-formats")
	if err != nil {
		return nil, fmt.Errorf("failed to get formats: %w", err)
	}
	info.Formats = formats

	return info, nil
}

// GetVersion returns the FFmpeg version string
func (p *FFmpegProbe) GetVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.ffmpegPath, "-version")
	output, err := cmd.Output()
	if err != nil {
		// Capture stderr for debugging if available
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		return "", fmt.Errorf("failed to execute ffmpeg -version: %w%s", err, formatStderr(stderr))
	}

	// Parse the first line which contains the version
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	if scanner.Scan() {
		line := scanner.Text()
		// Try to extract version using regex for more robustness
		version := extractVersion(line)
		if version != "" {
			return version, nil
		}
		// Fallback: return the whole line if regex fails
		return line, nil
	}

	return "", fmt.Errorf("failed to parse ffmpeg version output")
}

// extractVersion uses regex to extract version from ffmpeg output
// Handles formats like:
// - "ffmpeg version 4.4.2-0ubuntu0.22.04.1"
// - "ffmpeg version n8.1"
// - "ffmpeg version git-2024-01-15-abcdef"
func extractVersion(line string) string {
	// Regex to match "ffmpeg version" followed by version string
	re := regexp.MustCompile(`^ffmpeg version\s+(\S+)`)
	matches := re.FindStringSubmatch(line)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// GetEncoders returns a list of available encoders
func (p *FFmpegProbe) GetEncoders(ctx context.Context) ([]CodecInfo, error) {
	return p.getCodecList(ctx, "-encoders")
}

// GetDecoders returns a list of available decoders
func (p *FFmpegProbe) GetDecoders(ctx context.Context) ([]CodecInfo, error) {
	return p.getCodecList(ctx, "-decoders")
}

// getCodecList parses encoder or decoder list from ffmpeg output
func (p *FFmpegProbe) getCodecList(ctx context.Context, flag string) ([]CodecInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.ffmpegPath, flag)
	output, err := cmd.Output()
	if err != nil {
		// Capture stderr for debugging if available
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		return nil, fmt.Errorf("failed to execute ffmpeg %s: %w%s", flag, err, formatStderr(stderr))
	}

	return parseCodecOutput(string(output)), nil
}

// getRawInfo executes ffmpeg with an info flag and returns the raw text output.
// Used for P0/P1 flags like -hwaccels, -codecs, -filters, -pix_fmts, -formats.
func (p *FFmpegProbe) getRawInfo(ctx context.Context, flag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.ffmpegPath, flag)
	output, err := cmd.Output()
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		return "", fmt.Errorf("failed to execute ffmpeg %s: %w%s", flag, err, formatStderr(stderr))
	}

	return string(output), nil
}

// formatStderr formats stderr for inclusion in error messages
func formatStderr(stderr string) string {
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf("\nstderr: %s", stderr)
}

// parseCodecOutput parses the output of ffmpeg -encoders or -decoders
// Output format:
//
//	V..... = Video
//	A..... = Audio
//	S..... = Subtitle
//	------
//	V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
//	V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
func parseCodecOutput(output string) []CodecInfo {
	var codecs []CodecInfo

	// Regex to match codec lines
	// Format: " V....D name               description"
	// The first character is a space, followed by V/A/S, then flags (6 chars total), then name, then description
	// We need to exclude lines like " V..... = Video" which have "=" after the flags
	// Valid codec lines have the format: " X..... name description" where X is V/A/S
	// Use [^ ]{5} instead of [.A-Z]{5} to be more flexible with flag characters
	// Use [a-zA-Z0-9_][^\s]* to allow codec names starting with letters, digits, or underscore
	// This excludes lines where name is just "=" (header separator lines)
	codecLineRegex := regexp.MustCompile(`^\s([VAS])[^ ]{5}\s+([a-zA-Z0-9_][^\s]*)\s+(.+)$`)

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()

		matches := codecLineRegex.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		codecType := CodecType(matches[1])
		name := matches[2]
		description := strings.TrimSpace(matches[3])

		codec := CodecInfo{
			Name:        name,
			Description: description,
			Type:        codecType,
			IsHW:        isHardwareCodec(name),
		}

		codecs = append(codecs, codec)
	}

	return codecs
}

// isHardwareCodec checks if a codec name indicates hardware acceleration
// Hardware-accelerated codecs typically have suffixes like:
// - _nvenc (NVIDIA)
// - _qsv (Intel Quick Sync Video)
// - _vaapi (VA-API, Linux)
// - _amf (AMD AMF)
// - _videotoolbox (macOS)
// - _cuvid (NVIDIA decode)
// - _vdpau (VDPAU, Linux)
func isHardwareCodec(name string) bool {
	hwSuffixes := []string{
		"_nvenc",
		"_qsv",
		"_vaapi",
		"_amf",
		"_videotoolbox",
		"_cuvid",
		"_vdpau",
		"_nvdec",
	}

	for _, suffix := range hwSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}

	return false
}

// GetVideoEncoders returns only video encoders
func (info *FFmpegInfo) GetVideoEncoders() []CodecInfo {
	var result []CodecInfo
	for _, c := range info.Encoders {
		if c.Type == CodecTypeVideo {
			result = append(result, c)
		}
	}
	return result
}

// GetVideoDecoders returns only video decoders
func (info *FFmpegInfo) GetVideoDecoders() []CodecInfo {
	var result []CodecInfo
	for _, c := range info.Decoders {
		if c.Type == CodecTypeVideo {
			result = append(result, c)
		}
	}
	return result
}

// GetHWEncoders returns only hardware-accelerated encoders
func (info *FFmpegInfo) GetHWEncoders() []CodecInfo {
	var result []CodecInfo
	for _, c := range info.Encoders {
		if c.IsHW {
			result = append(result, c)
		}
	}
	return result
}

// GetHWDecoders returns only hardware-accelerated decoders
func (info *FFmpegInfo) GetHWDecoders() []CodecInfo {
	var result []CodecInfo
	for _, c := range info.Decoders {
		if c.IsHW {
			result = append(result, c)
		}
	}
	return result
}

// EncoderNames returns a list of encoder names (for WorkerCapabilities)
func (info *FFmpegInfo) EncoderNames() []string {
	names := make([]string, len(info.Encoders))
	for i, c := range info.Encoders {
		names[i] = c.Name
	}
	return names
}

// DecoderNames returns a list of decoder names (for WorkerCapabilities)
func (info *FFmpegInfo) DecoderNames() []string {
	names := make([]string, len(info.Decoders))
	for i, c := range info.Decoders {
		names[i] = c.Name
	}
	return names
}
