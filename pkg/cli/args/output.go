package args

import (
	"fmt"
	"path/filepath"

	"github.com/tsic404/rffmpeg/pkg/ffmpegopts"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
)

// OutputExtensionWarning returns a user-facing warning when the output file's
// extension cannot be mapped to an ffmpeg muxer and no output "-f <format>"
// is given. The worker-side ffmpeg may then fail after submission with the
// opaque "Unable to choose an output format" (e.g. for "out.out"), so the CLI
// surfaces guidance up front.
//
// It is a warning, not an error: the extension table is generated from a
// specific ffmpeg build while the muxer decision is made by the remote
// worker's ffmpeg, so the two may drift. Rejecting here could turn a job that
// the worker would accept into a hard failure; the worker stays the authority
// and this only hints.
//
// It returns "" when no hint is needed: streaming output ("-", handled by
// StreamingOutputError), an explicit output -f, a remote URL (ffmpeg applies
// scheme-specific defaults or requires -f), or a recognized extension.
func OutputExtensionWarning(r *ParseResult) string {
	if r.StreamingOutput {
		return ""
	}
	// An explicit output "-f <format>" selects the muxer, so the filename
	// extension is irrelevant (mirrors ValidateStreamingOutputFormat's scan
	// of the output section).
	if format, _ := ValidateStreamingOutputFormat(r.AllArgs); format != "" {
		return ""
	}
	if pathutil.IsRemoteURL(r.OutputFile) {
		return ""
	}
	if ffmpegopts.IsKnownOutputExtension(filepath.Ext(r.OutputFile)) {
		return ""
	}
	return fmt.Sprintf(
		`Warning: cannot infer an output format for %q: %q is not a recognized muxer extension. Use a standard extension (e.g. .mp4 or .mkv) or specify the format explicitly with -f <format>, otherwise the remote ffmpeg may fail with "Unable to choose an output format".`,
		r.OutputFile, filepath.Ext(r.OutputFile),
	)
}
