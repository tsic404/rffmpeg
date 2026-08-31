// Package pathutil provides path-safety helpers shared by the server and the
// worker. Keeping the traversal check in one place (rather than a copy in each
// layer) ensures both sides always enforce the same semantic.
package pathutil

import (
	"regexp"
	"strings"
)

// ContainsPathTraversal reports whether any component of path is exactly ".."
// (a path-traversal attempt). Unlike a raw substring search, legitimate
// filenames such as "my..video.mp4" or "a..b/c.mp4" are not false-positived.
func ContainsPathTraversal(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}

// remoteURLPattern matches a URL scheme at the start of the string: an RFC 3986
// scheme followed by "://". The pattern is anchored at the start and no
// substring search is used, so a "://" inside an otherwise-local absolute path
// (e.g. /data/media/x://y) is a local path, not a remote URL (TSI-2646). The
// scheme is captured so callers can special-case known-local schemes.
var remoteURLPattern = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*)://`)

// IsRemoteURL reports whether s starts with a remote URL scheme. Absolute local
// paths never carry a leading scheme, so they return false even when a later
// path component contains "://".
//
// The "file" scheme is excluded: ffmpeg's file protocol addresses a local path,
// not a network endpoint. Treating "file://" as remote would let a direct-mode
// output like "file:///etc/cron.d/evil" bypass the shared-FS allow-list
// validation and be written by ffmpeg's native file protocol to an arbitrary
// local path (TSI-2646).
func IsRemoteURL(s string) bool {
	m := remoteURLPattern.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	return !strings.EqualFold(m[1], "file")
}
