package rewrite

import (
	"path"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/encoder"
	"github.com/tsic404/rffmpeg/pkg/ffmpegopts"
)

// OutputFormat returns the muxer name of the command's output: the
// output-section "-f" value when the command sets one, otherwise the muxer
// ffmpeg guesses from the output filename extension. Empty means the format
// cannot be determined (no output token, stdout without "-f", or an extension
// ffmpeg does not map to a muxer).
//
// Only the output section counts: a "-f lavfi" introducing an input must not
// be taken for the output format.
func OutputFormat(args []string) string {
	start := outputSectionStart(args)
	if start < 0 {
		return ""
	}
	if format := outputFormatFlag(args, start); format != "" {
		return format
	}
	output := lastPositional(args, start)
	if output == "" || output == "-" {
		return ""
	}
	return ffmpegopts.MuxerForExtension(path.Ext(output))
}

// outputSectionStart returns the index of the first argument after the last
// "-i <input>" pair, or -1 when the command names no input.
func outputSectionStart(args []string) int {
	last := -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" {
			last = i + 1
		}
	}
	if last < 0 {
		return -1
	}
	return last + 1
}

// outputFormatFlag returns the last output-section "-f" value, lowercased;
// empty when the command sets none.
func outputFormatFlag(args []string, start int) string {
	format := ""
	for i := start; i+1 < len(args); i++ {
		if args[i] == "-f" {
			format = strings.ToLower(args[i+1])
		}
	}
	return format
}

// lastPositional returns the last output-section token that is neither an
// option nor an option's value: ffmpeg's output file. Options sharing their
// value in one token ("-ss1") and boolean flags consume no following token;
// everything else takes the next token as its value.
func lastPositional(args []string, start int) string {
	positional := ""
	for i := start; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			continue
		}
		if len(arg) > 1 && arg[0] == '-' {
			name := arg[1:]
			if strings.IndexByte(name, '=') >= 0 {
				continue
			}
			if !ffmpegopts.IsBoolean(name) && i+1 < len(args) {
				i++
			}
			continue
		}
		positional = arg
	}
	return positional
}

// ContainerCodecFamily maps the codec name ffmpeg reports as a muxer's default
// video codec (see ffmpegopts.DefaultVideoCodec) to the codec family the
// rewrite engine models. Families outside the modelled set report empty: the
// engine has no substitutable encoder for them, so the command is left to
// ffmpeg's own default (C-72).
func ContainerCodecFamily(codec string) encoder.CodecFormat {
	switch codec {
	case "h264":
		return encoder.CodecH264
	case "hevc":
		return encoder.CodecHEVC
	case "vp9":
		return encoder.CodecVP9
	case "av1":
		return encoder.CodecAV1
	case "vp8":
		return encoder.CodecVP8
	case "mpeg2video":
		return encoder.CodecMPEG2
	case "mpeg4":
		return encoder.CodecMPEG4
	default:
		return ""
	}
}

// containerTargetCodec returns the target of a request that names no encoder:
// the default video codec family of the command's effective output format,
// provided this worker can run at least one encoder of that family. format is
// the muxer the family was derived from. ok=false means the command must be
// left to ffmpeg untouched.
func containerTargetCodec(args []string, hwCaps *HardwareCapabilities) (format string, codec encoder.CodecFormat, ok bool) {
	format = OutputFormat(args)
	if format == "" {
		return "", "", false
	}
	codecName, carriesVideo := ffmpegopts.DefaultVideoCodec(format)
	if !carriesVideo {
		return "", "", false
	}
	family := ContainerCodecFamily(codecName)
	if family == "" {
		return "", "", false
	}
	return format, family, hasUsableEncoder(hwCaps, family)
}

// hasUsableEncoder reports whether the worker can run an encoder of family:
// an available hardware encoder, or the family's software encoder.
func hasUsableEncoder(hwCaps *HardwareCapabilities, family encoder.CodecFormat) bool {
	if hwCaps.GetBestHardwareEncoder(family) != "" {
		return true
	}
	if hwCaps.GetSoftwareEncoder(family) != "" {
		return true
	}
	sw := SoftwareEncoderForCodec(family)
	return sw != "" && hwCaps.HasEncoder(sw)
}

// HasNoVideoOutput reports whether the command produces no video output:
// "-vn" disables video entirely, and "-map" selections that never target a
// video stream exclude it. A "-map" naming a whole input, a video stream or an
// untyped stream index may select video, so it does not count as exclusion.
// Flags after "--" are passed through and do not count.
func HasNoVideoOutput(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "-vn" {
			return true
		}
	}
	return mapExcludesVideo(args)
}

// mapExcludesVideo reports whether the command has positive "-map" selections
// and none of them can include a video stream.
func mapExcludesVideo(args []string) bool {
	hasMap := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--" {
			break
		}
		if args[i] != "-map" {
			continue
		}
		spec := args[i+1]
		if spec == "" || strings.HasPrefix(spec, "-") {
			continue // negative map removes streams, it never adds video
		}
		hasMap = true
		if mapMaySelectVideo(spec) {
			return false
		}
	}
	return hasMap
}

// mapMaySelectVideo reports whether a positive "-map" stream specifier can
// select video: a bare input index maps every stream, "v"/"V" select video,
// and an untyped stream index is unknown (treated as possibly video).
func mapMaySelectVideo(spec string) bool {
	spec = strings.TrimSuffix(spec, "?")
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) == 1 || parts[1] == "" {
		return true
	}
	switch parts[1][0] {
	case 'a', 's', 'd', 't':
		return false
	default:
		return true
	}
}
