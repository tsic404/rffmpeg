// Package pathutil provides path-safety helpers shared by the server and the
// worker. Keeping the traversal check in one place (rather than a copy in each
// layer) ensures both sides always enforce the same semantic.
package pathutil

import "strings"

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
