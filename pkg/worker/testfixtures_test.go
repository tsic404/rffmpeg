package worker

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testMediaMP4 is a genuine MP4 (H.264, 64x64, two frames, 0.2s) generated
// with ffmpeg's lavfi testsrc. Worker fixtures serve it as the job input so
// the real ffprobe duration probe parses the same stream shape production
// sees, instead of rejecting synthetic bytes.
//
//go:embed testdata/tiny.mp4
var testMediaMP4 []byte

// testMediaBytes returns the shared real-media input payload. The returned
// slice is read-only; callers must not mutate it.
func testMediaBytes() []byte {
	return testMediaMP4
}

// writeShellFixture writes body as a script interpreted by posixSh. The -n
// pass rejects scripts that are not valid POSIX-shell syntax. It does not
// prove runtime portability: a host whose posixSh is bash accepts bash-only
// syntax, and valid syntax can still call tools the host lacks.
func writeShellFixture(path, body string) error {
	script := "#!" + posixSh + "\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return err
	}
	out, err := exec.Command(posixSh, "-n", path).CombinedOutput()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("fixture interpreter %s is missing: %w", posixSh, err)
		}
		return fmt.Errorf("fixture %s rejected by %s -n: %v: %s", path, posixSh, err, out)
	}
	return nil
}

// TestWriteShellFixture pins the helper's contract: the written shebang is
// posixSh, a valid fixture runs under it, and invalid syntax is rejected at
// write time. The rejection is syntax-only (see writeShellFixture).
func TestWriteShellFixture(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "valid.sh")
	if err := writeShellFixture(valid, "printf '%s\\n' ok\n"); err != nil {
		t.Fatalf("writeShellFixture rejected a valid script: %v", err)
	}
	data, err := os.ReadFile(valid)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if first, _, _ := strings.Cut(string(data), "\n"); first != "#!"+posixSh {
		t.Errorf("fixture shebang = %q, want %q", first, "#!"+posixSh)
	}
	out, err := exec.Command(posixSh, valid).Output()
	if err != nil {
		t.Fatalf("execute valid fixture with %s: %v", posixSh, err)
	}
	if got := strings.TrimSpace(string(out)); got != "ok" {
		t.Errorf("valid fixture output = %q, want %q", got, "ok")
	}

	invalid := filepath.Join(dir, "invalid.sh")
	err = writeShellFixture(invalid, "if true; then\nprintf 'unterminated'\n")
	if err == nil {
		t.Fatal("writeShellFixture accepted a script with an unterminated if block")
	}
	if !strings.Contains(err.Error(), invalid) || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error does not name the rejected fixture: %v", err)
	}
}
