package rewrite

import (
	"strings"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// VideoEncoderFlag is a video encoder selection option found in an ffmpeg
// argument list. Name is the option spelling without the leading dash (for
// example "c:v:0"); Value is the encoder name it selects.
type VideoEncoderFlag struct {
	// Index is the position of the flag token in the argument list.
	Index int

	// Name is the flag spelling without the leading dash.
	Name string

	// Specifier is the stream specifier suffix after the video type: ""
	// for all video streams, "0" for "-c:v:0". Equivalent spellings
	// ("-c:v", "-codec:v", "-vcodec") share the empty specifier.
	Specifier string

	// Value is the encoder name.
	Value string

	// Generic reports the general "-c" / "-codec" form, which also selects
	// audio and subtitle codecs unless it names a known video encoder.
	Generic bool
}

// ParseVideoEncoderFlags returns every video encoder selection option in args.
//
// Explicit video flags (-c:v / -codec:v with an optional stream specifier, and
// -vcodec) always count. The general -c / -codec flag only counts when its
// value names a known video encoder: it otherwise addresses every stream type
// and must not steer the video rewrite. "copy" is reported as a selection so
// callers can pass it through unchanged. Parsing stops at the "--" separator,
// whose contents are passed to ffmpeg verbatim.
//
// The "=" joined spellings ("-c:v=libx264") are not selections: ffmpeg rejects
// that syntax, so callers must leave the command unchanged and let ffmpeg
// report the error (see HasInlineVideoEncoderSyntax).
func ParseVideoEncoderFlags(args []string, hwCaps *HardwareCapabilities) []VideoEncoderFlag {
	flags := make([]VideoEncoderFlag, 0, 2)
	for i := range args {
		arg := args[i]
		if arg == "--" {
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			continue
		}
		name := arg[1:]
		if strings.IndexByte(name, '=') >= 0 {
			continue
		}
		generic, specifier, ok := videoEncoderFlagName(name)
		if !ok {
			continue
		}
		if i+1 >= len(args) || args[i+1] == "" || args[i+1] == "-" || strings.HasPrefix(args[i+1], "-") {
			continue
		}
		value := args[i+1]
		if generic && value != "copy" && !knownVideoEncoder(value, hwCaps) {
			continue
		}
		flags = append(flags, VideoEncoderFlag{
			Index:     i,
			Name:      name,
			Specifier: specifier,
			Value:     value,
			Generic:   generic,
		})
	}
	return flags
}

// videoEncoderFlagName classifies an option name (leading dash stripped) and
// returns the video stream specifier. generic is true for the general -c /
// -codec form.
func videoEncoderFlagName(name string) (generic bool, specifier string, ok bool) {
	switch {
	case name == "vcodec":
		return false, "", true
	case strings.HasPrefix(name, "c:v:"):
		return false, name[len("c:v:"):], true
	case name == "c:v":
		return false, "", true
	case strings.HasPrefix(name, "codec:v:"):
		return false, name[len("codec:v:"):], true
	case name == "codec:v":
		return false, "", true
	case name == "c" || name == "codec":
		return true, "", true
	default:
		return false, "", false
	}
}

// knownVideoEncoder reports whether name is a video encoder this process knows
// about: one of the built-in encoder families, or one the worker reported.
func knownVideoEncoder(name string, hwCaps *HardwareCapabilities) bool {
	enc := encoder.EncoderFamily(name)
	if enc.CodecFormat() != "" {
		return true
	}
	if hwCaps != nil {
		for _, available := range hwCaps.AvailableEncoders {
			if available == enc {
				return true
			}
		}
	}
	return false
}

// PrimaryVideoEncoderFlag returns the selection that decides the video
// encoder. ffmpeg applies the last matching option, so the last explicit video
// declaration wins over earlier ones; a general "-c" / "-codec" selection is
// used only when no explicit video flag is present.
func PrimaryVideoEncoderFlag(flags []VideoEncoderFlag) (VideoEncoderFlag, bool) {
	for i := len(flags) - 1; i >= 0; i-- {
		if !flags[i].Generic {
			return flags[i], true
		}
	}
	if len(flags) > 0 {
		return flags[len(flags)-1], true
	}
	return VideoEncoderFlag{}, false
}

// RewrittenTokens renders this selection for target. The flag spelling is kept
// (including its stream specifier); the general form is narrowed to "-c:v"
// because the target video encoder cannot serve the other stream types.
func (f VideoEncoderFlag) RewrittenTokens(target encoder.EncoderFamily) []string {
	if f.Generic {
		return []string{"-c:v", string(target)}
	}
	return []string{"-" + f.Name, string(target)}
}

// HasInlineVideoEncoderSyntax reports whether args contain a video encoder
// option written with "=" ("-c:v=libx264", "-vcodec=libx264"). ffmpeg rejects
// that syntax; it is neither a selection nor a reason to auto-select, so the
// rewrite path must leave the command untouched and let ffmpeg fail. Options
// after "--" are passed through and do not count.
func HasInlineVideoEncoderSyntax(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if len(arg) < 2 || arg[0] != '-' {
			continue
		}
		name := arg[1:]
		eq := strings.IndexByte(name, '=')
		if eq < 0 {
			continue
		}
		if _, _, ok := videoEncoderFlagName(name[:eq]); ok {
			return true
		}
	}
	return false
}

// ReplacePrimaryVideoEncoder returns args with the primary video encoder
// declaration rewritten to enc, keeping that declaration's spelling and stream
// specifier. It is the single in-place replacement used by retry and fallback
// paths. All other tokens — including earlier overridden declarations, a
// general "-c copy", and arguments after "--" — are preserved verbatim. When
// no video encoder is declared, args is returned unchanged.
func ReplacePrimaryVideoEncoder(args []string, enc encoder.EncoderFamily, hwCaps *HardwareCapabilities) []string {
	flags := ParseVideoEncoderFlags(args, hwCaps)
	primary, ok := PrimaryVideoEncoderFlag(flags)
	if !ok || primary.Value == "copy" {
		return args
	}
	flagAt := make(map[int]VideoEncoderFlag, len(flags))
	for _, flag := range flags {
		flagAt[flag.Index] = flag
	}

	result := make([]string, 0, len(args)+1)
	skipNext := false
	passthrough := false
	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if passthrough {
			result = append(result, arg)
			continue
		}
		if arg == "--" {
			passthrough = true
			result = append(result, arg)
			continue
		}
		flag, isEncoder := flagAt[i]
		if !isEncoder {
			result = append(result, arg)
			continue
		}
		if flag.Index == primary.Index {
			result = append(result, flag.RewrittenTokens(enc)...)
		} else {
			result = append(result, arg, args[i+1])
		}
		skipNext = true
	}
	return result
}
