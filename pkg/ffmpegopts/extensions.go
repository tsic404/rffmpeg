package ffmpegopts

import "strings"

// IsKnownOutputExtension reports whether ext maps to a known ffmpeg output
// muxer. ext may carry a leading dot and any letter case; both are
// normalized before lookup. The extension table is generated from
// `ffmpeg -muxers` and `ffmpeg -h muxer=<name>` for every muxer (see
// muxers_gen.go).
func IsKnownOutputExtension(ext string) bool {
	return MuxerForExtension(ext) != ""
}

// MuxerForExtension returns the muxer ffmpeg guesses for an output filename
// extension: the first muxer in `ffmpeg -muxers` registration order that
// declares the extension. ext may carry a leading dot and any letter case.
// Empty means ffmpeg cannot infer an output format from the extension.
func MuxerForExtension(ext string) string {
	return extensionMuxer[strings.ToLower(strings.TrimPrefix(ext, "."))]
}

// DefaultVideoCodec returns the video codec native ffmpeg selects for muxer
// when the output request names no encoder. ok is false when the muxer cannot
// carry a video stream at all, or when the muxer is unknown to this build.
func DefaultVideoCodec(muxer string) (codec string, ok bool) {
	codec, ok = muxerDefaultVideoCodec[strings.ToLower(muxer)]
	return codec, ok
}
