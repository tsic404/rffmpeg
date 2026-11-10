package pathutil

import (
	"os"
	"path/filepath"
	"strings"
)

// Space is the deployment-declared shared-filesystem path space (C-107): a set
// of root paths. A candidate belongs to the space when its resolved form lies
// under one of the roots. An empty space has no legal path at all — the mode is
// fail-closed and never falls back to "any absolute path" (ADR-018 option A).
type Space struct {
	roots []string
}

// NewSpace parses a comma-separated root declaration. Entries are trimmed,
// cleaned, and symlink-resolved once so membership compares the same real paths
// on both the submission and the execution side. A relative entry can never
// match an absolute candidate and therefore contributes no legal path.
func NewSpace(raw string) *Space {
	s := &Space{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		cleaned := filepath.Clean(part)
		// A root that does not exist yet cannot be resolved; its literal form
		// still matches candidates judged below it.
		if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
			cleaned = resolved
		}
		s.roots = append(s.roots, cleaned)
	}
	return s
}

// Declared reports whether at least one root is declared. False means the mode
// has no legal path.
func (s *Space) Declared() bool {
	return s != nil && len(s.roots) > 0
}

// Roots returns the resolved root list (nil when nothing was declared).
func (s *Space) Roots() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.roots...)
}

// Contains reports whether path belongs to the space. Membership is judged on
// the resolved path, per the C-107 rule: the longest existing prefix of the
// path is resolved through symlinks, while the not-yet-existing tail is judged
// literally. A candidate carrying a ".." component is rejected outright — the
// rule excludes upward segments, and letting them through would make the
// literal tail judgement meaningless.
func (s *Space) Contains(path string) bool {
	if s == nil || len(s.roots) == 0 {
		return false
	}
	if !filepath.IsAbs(path) || ContainsPathTraversal(path) {
		return false
	}
	resolved, ok := ResolveExistingPrefix(filepath.Clean(path))
	if !ok {
		return false
	}
	for _, root := range s.roots {
		if resolved == root || strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// ResolveExistingPrefix resolves symlinks along the longest existing prefix of
// the absolute, traversal-free path and re-attaches the remaining segments
// literally (C-107). It reports false when nothing along the path can be
// resolved or when a resolved prefix is a broken symlink.
func ResolveExistingPrefix(cleaned string) (string, bool) {
	prefix := cleaned
	var tail []string
	for {
		if _, err := os.Lstat(prefix); err == nil {
			real, err := filepath.EvalSymlinks(prefix)
			if err != nil {
				return "", false
			}
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real, true
		} else if !os.IsNotExist(err) {
			return "", false
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return "", false
		}
		tail = append(tail, filepath.Base(prefix))
		prefix = parent
	}
}

// Within reports whether path resolves inside dir. Both sides are resolved with
// the same longest-existing-prefix rule, so a workspace that does not exist yet
// is still judged correctly. It is the C-126 workspace containment predicate.
func Within(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	realDir, ok := ResolveExistingPrefix(filepath.Clean(dir))
	if !ok {
		return false
	}
	realPath, ok := ResolveExistingPrefix(filepath.Clean(path))
	if !ok {
		return false
	}
	if realPath == realDir {
		return true
	}
	return strings.HasPrefix(realPath, realDir+string(os.PathSeparator))
}
