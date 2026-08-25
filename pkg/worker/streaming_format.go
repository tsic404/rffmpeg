package worker

import (
	"fmt"
	"strings"
)

// streamableMuxerFlags maps container formats whose muxers require a seekable
// output onto the ffmpeg flags that lift that requirement. The mp4/mov family
// writes the moov index at close time by seeking back, so piping to stdout
// fails with "muxer does not support non seekable output"; empty_moov plus
// fragmented output produces a valid streamable file instead.
var streamableMuxerFlags = map[string][]string{
	"mp4": {"-movflags", "+empty_moov+frag_keyframe+default_base_moof"},
	"mov": {"-movflags", "+empty_moov+frag_keyframe+default_base_moof"},
}

// applyStreamableFormat adjusts ffmpeg arguments for streaming (stdout) jobs:
// when the selected muxer requires a seekable output (mp4, mov) and the user
// has not configured movflags explicitly, inject the fragmented-output flags
// so the job succeeds instead of failing inside ffmpeg. The requested
// container is preserved — the format is never silently swapped.
//
// Returns the (possibly unchanged) args and a human-readable notification;
// the notification is empty when nothing was changed.
func applyStreamableFormat(args []string, outputPath string) ([]string, string) {
	if outputPath != "-" {
		return args, ""
	}

	// Input options precede the last "-i <value>" pair; anything after it may
	// belong to the output. Only an output-section "-f <format>" matters —
	// e.g. "-f lavfi -i testsrc" must not trigger the mp4 path.
	lastInputValue := -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" {
			lastInputValue = i + 1
		}
	}

	// The trailing element is the output path itself, so stop before it.
	for i := len(args) - 2; i > lastInputValue; i-- {
		if args[i] != "-f" {
			continue
		}
		format := strings.ToLower(args[i+1])
		flags, ok := streamableMuxerFlags[format]
		if !ok {
			// A different output muxer (mpegts, matroska, flv, …) — either
			// already streamable or unknown; leave the command untouched.
			return args, ""
		}
		// Explicit user movflags win — never override them.
		for _, arg := range args {
			if arg == "-movflags" {
				return args, ""
			}
		}
		updated := make([]string, 0, len(args)+len(flags))
		updated = append(updated, args[:i+2]...)
		updated = append(updated, flags...)
		updated = append(updated, args[i+2:]...)
		note := fmt.Sprintf(
			"streaming output: %s muxer requires seekable output; auto-enabled fragmented output (-movflags %s)",
			format, strings.TrimPrefix(flags[1], "+"),
		)
		return updated, note
	}
	return args, ""
}
