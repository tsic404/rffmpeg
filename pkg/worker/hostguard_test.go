package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// posixSh is the interpreter every shell fixture declares in its shebang.
// Fixtures and the preflight validate this exact path, never a PATH lookup, so
// the two cannot disagree about which shell runs them.
const posixSh = "/bin/sh"

// TestMain gates the package on the fixture interpreter before any test runs:
// without posixSh every shell-backed fixture would fail on its own, reading as
// a product defect rather than a host gap. procfs is not gated here — only the
// priority assertion needs it, and that test skips when procfs is absent.
func TestMain(m *testing.M) {
	if err := preflightFixtureInterpreter(); err != nil {
		fmt.Fprintf(os.Stderr, "worker test harness: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// preflightFixtureInterpreter proves posixSh exists and executes a POSIX
// script. It checks the interpreter only; per-fixture syntax validation stays
// in writeShellFixture.
func preflightFixtureInterpreter() error {
	if _, err := os.Stat(posixSh); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fixture interpreter %s is missing: %w", posixSh, err)
		}
		return fmt.Errorf("stat fixture interpreter %s: %w", posixSh, err)
	}
	const probe = `i=0; while [ $i -lt 2 ]; do i=$((i+1)); done; printf '%s' "$i"`
	out, err := exec.Command(posixSh, "-c", probe).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s cannot run a POSIX script: %w: %s", posixSh, err, out)
	}
	if strings.TrimSpace(string(out)) != "2" {
		return fmt.Errorf("%s probe printed %q, want \"2\"", posixSh, out)
	}
	return nil
}
