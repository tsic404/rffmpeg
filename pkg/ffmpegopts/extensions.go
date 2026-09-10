package ffmpegopts

import "strings"

// IsKnownOutputExtension reports whether ext maps to a known ffmpeg output
// muxer. ext may carry a leading dot and any letter case; both are
// normalized before lookup. The extension table is generated from
// `ffmpeg -h muxer=<name>` for every muxer (see extensions_gen.go).
func IsKnownOutputExtension(ext string) bool {
	ext = strings.TrimPrefix(ext, ".")
	return knownOutputExtensions[strings.ToLower(ext)]
}
