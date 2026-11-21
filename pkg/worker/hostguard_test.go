package worker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// posixSh is the interpreter every shell fixture declares in its shebang.
// Fixtures and the preflight validate this exact path, never a PATH lookup, so
// the two cannot disagree about which shell runs them.
const posixSh = "/bin/sh"

// bashFixture is the interpreter a fixture needs when it dials with bash's
// /dev/tcp or /dev/udp redirections. A POSIX shell without those redirections
// fails the redirection itself, so the script never reaches the code that
// reports a failed connection and the worker reads as "never dialled".
const bashFixture = "/bin/bash"

// TestMain gates the package on the fixture interpreter before any test runs:
// without posixSh every shell-backed fixture would fail on its own, reading as
// a product defect rather than a host gap. procfs is not gated here — only the
// priority assertion needs it, and that test skips when procfs is absent.
func TestMain(m *testing.M) {
	if err := preflightFixtureInterpreter(); err != nil {
		fmt.Fprintf(os.Stderr, "worker test harness: %v\n", err)
		os.Exit(1)
	}
	if err := preflightBashFixture(); err != nil {
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

// preflightBashFixture proves that a fixture can open a socket through bash's
// /dev/tcp redirection. Fixtures that really dial the worker's relay depend on
// it; proving it here keeps a host without the redirection from looking like a
// worker that never connected.
func preflightBashFixture() error {
	if _, err := os.Stat(bashFixture); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fixture interpreter %s is missing: %w", bashFixture, err)
		}
		return fmt.Errorf("stat fixture interpreter %s: %w", bashFixture, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for the %s /dev/tcp probe: %w", bashFixture, err)
	}
	defer listener.Close()
	go func() {
		if conn, err := listener.Accept(); err == nil {
			conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	probe := fmt.Sprintf(`exec 3<>"/dev/tcp/127.0.0.1/%d"`, port)
	out, err := exec.Command(bashFixture, "-c", probe).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s cannot open /dev/tcp: %w: %s", bashFixture, err, out)
	}
	return nil
}
