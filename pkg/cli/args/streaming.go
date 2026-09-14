package args

import (
	"fmt"
	"strings"
)

// nonStreamableMuxers lists container formats whose muxers cannot write to a
// pipe and cannot be made streamable by the worker's movflags injection,
// which only covers mp4/mov. Rejecting them up front yields a friendly error
// instead of ffmpeg's opaque "Error initializing the muxer for pipe:: Invalid
// argument". Some entries (aac, tg2) are not muxers at all; the rest fail
// piping on any source, and 3gp/3g2 are mov-family but excluded because the
// auto-fix covers only mp4/mov.
var nonStreamableMuxers = map[string]bool{
	"avif": true,
	"f4v":  true,
	"ipod": true,
	"psp":  true,
	"isma": true,
	"m4a":  true,
	"alac": true,
	"m2v":  true,
	"tgp":  true,
	"aac":  true,
	"3gp":  true,
	"3g2":  true,
	"tg2":  true,
}

// ValidateStreamingOutputFormat reports whether a streaming (stdout) job's
// output-section muxer can be written to a pipe. It returns the detected
// format — empty when ffmpeg would have to auto-detect it, which it cannot do
// for "pipe:" ("Unable to choose an output format for 'pipe:'") — and
// ok=false when the output cannot be streamed. Only "-f <fmt>" flags after
// the last "-i <value>" pair are considered, mirroring the worker's scan so
// an input-section "-f lavfi" is never mistaken for the output format.
func ValidateStreamingOutputFormat(allArgs []string) (format string, ok bool) {
	lastInputValue := -1
	for i := 0; i+1 < len(allArgs); i++ {
		if allArgs[i] == "-i" {
			lastInputValue = i + 1
		}
	}

	// Scan backwards: the last output-section "-f" wins, matching ffmpeg's
	// own precedence. The i >= 1 guard keeps args[i-1] in bounds when there
	// is no "-i" pair (positional input) in allArgs.
	for i := len(allArgs) - 1; i > lastInputValue && i >= 1; i-- {
		if allArgs[i-1] != "-f" {
			continue
		}
		format = strings.ToLower(allArgs[i])
		return format, !nonStreamableMuxers[format]
	}

	// No output muxer selected: ffmpeg cannot auto-detect a format for
	// "pipe:".
	return "", false
}

// StreamingOutputError returns a user-facing message for a streaming
// (stdout) job the remote worker cannot complete, or "" when the request is
// streamable (or not a streaming job at all).
func StreamingOutputError(r *ParseResult) string {
	if !r.StreamingOutput {
		return ""
	}

	format, ok := ValidateStreamingOutputFormat(r.AllArgs)
	if ok {
		return ""
	}

	if format == "" {
		return `Error: output to stdout ("-") is not supported: remote transcoding stores output on server storage and ffmpeg cannot auto-detect a muxer for a pipe. Use a server-writable output path (e.g. output.mp4) or -o <local path>.`
	}
	return fmt.Sprintf(`Error: output to stdout ("-") is not supported for format %s: remote transcoding stores output on server storage and this muxer cannot write to a pipe. Use a server-writable output path (e.g. output.mp4) or -o <local path>.`, format)
}
