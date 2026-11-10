package pathutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpaceMembership covers the C-107 membership judgement: the longest
// existing prefix is resolved through symlinks while a not-yet-existing tail is
// judged literally, and an empty declaration has no legal path at all.
func TestSpaceMembership(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	outside := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the space that points outside it: the resolved path is
	// what membership is judged on, so it must not be a member.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	space := NewSpace("  " + root + " , " + outside + "-sibling ")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"root itself", root, true},
		{"inside", filepath.Join(root, "a.mp4"), true},
		{"inside nested", filepath.Join(root, "sub", "a.mp4"), true},
		{"not yet existing tail", filepath.Join(root, "out", "final.mp4"), true},
		{"sibling sharing the bare prefix string", root + "-sibling/x.mp4", false},
		{"outside every root", filepath.Join(t.TempDir(), "x.mp4"), false},
		{"relative path", "sub/a.mp4", false},
		{"empty", "", false},
		{"traversal inside the root lexically", root + "/../" + filepath.Base(root) + "/x.mp4", false},
		{"symlink pointing outside the space", filepath.Join(link, "x.mp4"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := space.Contains(tc.path); got != tc.want {
				t.Errorf("Space.Contains(%q) = %v, want %v (resolved root %q)", tc.path, got, tc.want, resolvedRoot)
			}
		})
	}
}

func TestSpaceUndeclaredIsFailClosed(t *testing.T) {
	for _, raw := range []string{"", "   ", ",,"} {
		space := NewSpace(raw)
		if space.Declared() {
			t.Fatalf("NewSpace(%q).Declared() = true, want false", raw)
		}
		if space.Contains("/etc/passwd") {
			t.Fatalf("NewSpace(%q) accepted an absolute path; an undeclared space has no legal path", raw)
		}
	}
	var nilSpace *Space
	if nilSpace.Declared() || nilSpace.Contains("/tmp/x") {
		t.Fatal("nil Space must be closed")
	}
}

func TestClassifyInput(t *testing.T) {
	root := t.TempDir()
	space := NewSpace(root)
	hexID := strings.Repeat("ab", 32) // 64 lowercase hex characters

	cases := []struct {
		name       string
		identifier string
		want       InputForm
		wantErr    bool
	}{
		{"upload identifier", hexID, InputUpload, false},
		{"http url", "http://example.com/a.mp4", InputRemoteURL, false},
		{"https url", "https://cdn.example.com/a/b.mkv", InputRemoteURL, false},
		{"absolute path in space", filepath.Join(root, "media", "a.mp4"), InputSharedPath, false},
		{"absolute path outside space", "/etc/passwd", InputInvalid, true},
		{"relative path", "media/a.mp4", InputInvalid, true},
		{"file url", "file:///etc/passwd", InputInvalid, true},
		{"non-http scheme", "rtmp://example.com/live", InputInvalid, true},
		{"upload id with query and scheme", hexID + "?a://../../../../etc/cron.d/x", InputInvalid, true},
		{"upload id with query only", hexID + "?x=1", InputInvalid, true},
		{"short hex", "abcdef", InputInvalid, true},
		{"uppercase hex", strings.Repeat("AB", 32), InputInvalid, true},
		{"http without host", "http:///etc/passwd", InputInvalid, true},
		{"empty", "", InputInvalid, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form, err := ClassifyInput(tc.identifier, space)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ClassifyInput(%q) = %v, want an error", tc.identifier, form)
				}
				if form != InputInvalid {
					t.Errorf("ClassifyInput(%q) form = %v, want InputInvalid", tc.identifier, form)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClassifyInput(%q): %v", tc.identifier, err)
			}
			if form != tc.want {
				t.Errorf("ClassifyInput(%q) = %v, want %v", tc.identifier, form, tc.want)
			}
		})
	}
}

// TestClassifyInputWithoutSpace locks the fail-closed default: with no declared
// root the third form simply does not exist, so an absolute path can never be
// classified as a shared filesystem input.
func TestClassifyInputWithoutSpace(t *testing.T) {
	form, err := ClassifyInput("/srv/media/a.mp4", NewSpace(""))
	if err == nil || form != InputInvalid {
		t.Fatalf("ClassifyInput(absolute path, undeclared space) = %v, %v; want InputInvalid with an error", form, err)
	}
}

func TestClassifyOutputTarget(t *testing.T) {
	root := t.TempDir()
	space := NewSpace(root)

	nonDirect := []struct {
		name    string
		wantErr bool
	}{
		{"out.mp4", false},
		{"my..video.mp4", false},
		{"", true},
		{"/etc/passwd", true},
		{"sub/out.mp4", true},
		{`sub\out.mp4`, true},
		{"..", true},
		{"../out.mp4", true},
		{"a/../b.mp4", true},
		// A protocol address is a target class of its own, not a file name
		// (C-125 / C-129).
		{"rtmp://example.com/live", false},
		{"srt://host:9000?mode=caller", false},
		{"udp://10.0.0.1:1234", false},
		// The file scheme addresses a local path, so it stays a rejected
		// file target in both classes.
		{"file:///etc/passwd", true},
		{"file://host/tmp/x.mp4", true},
		// A scheme with no host is not a protocol address target either.
		{"rtmp://", true},
		{"rtmp:///only/path", true},
		{"://example.com/live", true},
	}
	for _, tc := range nonDirect {
		form, err := ClassifyOutputTarget(tc.name, false, space)
		if (err != nil) != tc.wantErr {
			t.Errorf("ClassifyOutputTarget(%q, non-direct) err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
		if err == nil && form != OutputTargetFile && form != OutputTargetProtocolAddress {
			t.Errorf("ClassifyOutputTarget(%q, non-direct) = %v, want a classified form", tc.name, form)
		}
	}

	direct := []struct {
		name    string
		wantErr bool
	}{
		{filepath.Join(root, "out.mp4"), false},
		{filepath.Join(root, "new", "out.mp4"), false},
		{"", true},
		{"out.mp4", true},
		{filepath.Join(root, "..", "out.mp4"), true},
		{"/etc/passwd", true},
		{filepath.Join(root, "out.mp4") + "://x", false}, // a plain path whose tail contains "://" is still a path
		{"rtmp://example.com/live", true},                // a protocol address is not a pass-through target
	}
	for _, tc := range direct {
		form, err := ClassifyOutputTarget(tc.name, true, space)
		if (err != nil) != tc.wantErr {
			t.Errorf("ClassifyOutputTarget(%q, direct) err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
		if err == nil && form != OutputTargetFile {
			t.Errorf("ClassifyOutputTarget(%q, direct) = %v, want OutputTargetFile", tc.name, form)
		}
	}
}

func TestWithin(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "job"), 0o755); err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(dir, "job")
	if err := os.WriteFile(filepath.Join(jobDir, "input-0"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Within(jobDir, filepath.Join(jobDir, "input-0")) {
		t.Error("input inside the workspace must be contained")
	}
	if !Within(jobDir, filepath.Join(jobDir, "new.mp4")) {
		t.Error("a not-yet-existing file directly under the workspace must be contained")
	}
	if Within(jobDir, filepath.Join(dir, "outside.mp4")) {
		t.Error("a path outside the workspace must not be contained")
	}
	if Within(jobDir, jobDir+"-sibling/x.mp4") {
		t.Error("a sibling sharing the bare prefix must not be contained")
	}
}
